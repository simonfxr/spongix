package main

import (
	"context"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/folbricht/desync"
	"github.com/nix-community/go-nix/pkg/nar"
	"github.com/pascaldekloe/metrics"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

var (
	metricChunkCount   = metrics.MustInteger("spongix_chunk_count_local", "Number of chunks")
	metricChunkGcCount = metrics.MustCounter("spongix_chunk_gc_count_local", "Number of chunks deleted by GC")
	metricChunkGcSize  = metrics.MustCounter("spongix_chunk_gc_bytes_local", "Size of chunks deleted by GC")
	metricChunkSize    = metrics.MustInteger("spongix_chunk_size_local", "Size of the chunks in bytes")
	metricChunkWalk    = metrics.MustCounter("spongix_chunk_walk_local", "Total time spent walking the cache in ms")
	metricChunkDirs    = metrics.MustInteger("spongix_chunk_dir_count", "Number of directories the chunks are stored in")

	metricIndexCount   = metrics.MustInteger("spongix_index_count_local", "Number of indices")
	metricIndexGcCount = metrics.MustCounter("spongix_index_gc_count_local", "Number of indices deleted by GC")
	metricIndexWalk    = metrics.MustCounter("spongix_index_walk_local", "Total time spent walking the index in ms")

	metricInflated   = metrics.MustInteger("spongix_inflated_size_local", "Size of cache in bytes contents if they were inflated")
	metricMaxSize    = metrics.MustInteger("spongix_max_size_local", "Limit for the local cache in bytes")
	metricGcTime     = metrics.MustCounter("spongix_gc_time_local", "Total time spent in GC")
	metricVerifyTime = metrics.MustCounter("spongix_verify_time_local", "Total time spent in verification")
)

var yes = struct{}{}

func measure(metric *metrics.Counter, f func()) {
	start := time.Now()
	f()
	metric.Add(uint64(time.Since(start).Milliseconds()))
}

func (proxy *Proxy) gc() {
	proxy.log.Debug("Initializing GC", zap.Duration("interval", proxy.GcInterval))
	cacheStat := map[string]*chunkStat{}
	measure(metricGcTime, func() { proxy.gcOnce(cacheStat) })

	ticker := time.NewTicker(proxy.GcInterval)
	for {
		<-ticker.C
		measure(metricGcTime, func() { proxy.gcOnce(cacheStat) })
	}
}

func (proxy *Proxy) verify() {
	proxy.log.Debug("Initializing Verifier", zap.Duration("interval", proxy.VerifyInterval))
	measure(metricVerifyTime, func() { proxy.verifyOnce() })

	ticker := time.NewTicker(proxy.VerifyInterval)
	for {
		<-ticker.C
		measure(metricVerifyTime, func() { proxy.verifyOnce() })
	}
}

func (proxy *Proxy) verifyOnce() {
	proxy.log.Info("store verify started")
	store := proxy.localStore.(desync.LocalStore)
	err := store.Verify(context.Background(), runtime.GOMAXPROCS(0), true, os.Stderr)

	if err != nil {
		proxy.log.Error("store verify failed", zap.Error(err))
	} else {
		proxy.log.Info("store verify completed")
	}
}

type chunkStat struct {
	id    desync.ChunkID
	size  int64
	mtime time.Time
}

type chunkLRU struct {
	live        []*chunkStat
	liveSize    uint64
	liveSizeMax uint64
	dead        map[desync.ChunkID]struct{}
	deadSize    uint64
}

func NewLRU(liveSizeMax uint64) *chunkLRU {
	return &chunkLRU{
		live:        []*chunkStat{},
		liveSizeMax: liveSizeMax,
		dead:        map[desync.ChunkID]struct{}{},
	}
}

func (l *chunkLRU) AddDead(stat *chunkStat) {
	l.dead[stat.id] = yes
	l.deadSize += uint64(stat.size)
}

func (l *chunkLRU) Add(stat *chunkStat) {
	isOlder := func(i int) bool { return l.live[i].mtime.Before(stat.mtime) }
	i := sort.Search(len(l.live), isOlder)
	l.insertAt(i, stat)
	l.liveSize += uint64(stat.size)
	for l.liveSize > l.liveSizeMax {
		die := l.live[len(l.live)-1]
		l.dead[die.id] = yes
		l.live = l.live[:len(l.live)-1]
		l.deadSize += uint64(die.size)
		l.liveSize -= uint64(die.size)
	}
}

func (l *chunkLRU) insertAt(i int, v *chunkStat) {
	if i == len(l.live) {
		l.live = append(l.live, v)
	} else {
		l.live = append(l.live[:i+1], l.live[i:]...)
		l.live[i] = v
	}
}

func (l *chunkLRU) IsDead(id desync.ChunkID) bool {
	_, found := l.dead[id]
	return found
}

func (l *chunkLRU) Dead() map[desync.ChunkID]struct{} {
	return l.dead
}

// we assume every directory requires 4KB of size (one block) desync stores
// files in directories with a 4 hex prefix, so we need to keep at least this
// amount of space reserved.
const maxCacheDirPortion = 0xffff * 4096

type integrityCheck struct {
	path  string
	index desync.Index
}

func checkNarContents(store desync.Store, idx desync.Index) error {
	buf := newAssembler(store, idx)
	narRd, err := nar.NewReader(buf)
	if err != nil {
		return err
	}
	none := true
	for {
		if _, err := narRd.Next(); err == nil {
			none = false
		} else if err == io.EOF {
			break
		} else {
			return err
		}
	}

	if none {
		return errors.New("no contents in NAR")
	}

	return nil
}

/*
Local GC strategies:
  Check every index file:
    If chunks are missing, delete it.
  	If it is not referenced by the database anymore, delete it.
  Check every narinfo in the database:
    If index is missing, delete it.
  	If last access is too old, delete it.
*/
func (proxy *Proxy) gcOnce(cacheStat map[string]*chunkStat) {
	maxCacheSize := (uint64(math.Pow(2, 30)) * proxy.CacheSize) - maxCacheDirPortion
	store := proxy.localStore.(desync.LocalStore)
	indices := proxy.localIndex.(desync.LocalIndexStore)
	lru := NewLRU(maxCacheSize)
	walkStoreStart := time.Now()
	chunkDirs := int64(0)

	metricMaxSize.Set(int64(maxCacheSize))

	// filepath.Walk is faster for our usecase because we need the stat result anyway.
	walkStoreErr := filepath.Walk(store.Base, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			if err == os.ErrNotExist {
				return nil
			} else {
				return err
			}
		}

		if info.IsDir() {
			chunkDirs++
			return nil
		}

		name := info.Name()
		if strings.HasPrefix(name, ".tmp") {
			return nil
		}

		ext := filepath.Ext(name)
		if ext != desync.CompressedChunkExt {
			return nil
		}

		idstr := name[0 : len(name)-len(ext)]

		id, err := desync.ChunkIDFromString(idstr)
		if err != nil {
			return err
		}

		stat := &chunkStat{id: id, size: info.Size(), mtime: info.ModTime()}

		if _, err := store.GetChunk(id); err != nil {
			proxy.log.Error("getting chunk", zap.Error(err), zap.String("chunk", id.String()))
			lru.AddDead(stat)
		} else {
			lru.Add(stat)
		}

		return nil
	})

	metricChunkWalk.Add(uint64(time.Since(walkStoreStart).Milliseconds()))
	metricChunkDirs.Set(chunkDirs)

	if walkStoreErr != nil {
		proxy.log.Error("While walking store", zap.Error(walkStoreErr))
		return
	}

	metricChunkCount.Set(int64(len(lru.live)))
	metricChunkGcCount.Add(uint64(len(lru.dead)))
	metricChunkGcSize.Add(lru.deadSize)
	metricChunkSize.Set(int64(lru.liveSize))

	deadIndices := &sync.Map{}
	walkIndicesStart := time.Now()
	indicesCount := int64(0)
	inflatedSize := int64(0)
	ignoreBeforeTime := time.Now().Add(10 * time.Minute)

	integrity := make(chan integrityCheck)
	wg := &sync.WaitGroup{}

	for i := 0; i < 3; i++ {
		wg.Add(1)

		go func(n int) {
			defer wg.Done()

			for {
				select {
				case <-time.After(1 * time.Second):
					return
				case check := <-integrity:
					switch filepath.Ext(check.path) {
					case ".nar":
						if err := checkNarContents(store, check.index); err != nil {
							proxy.log.Error("checking NAR contents", zap.Error(err), zap.String("path", check.path))
							deadIndices.Store(check.path, yes)
							continue
						}
					case ".narinfo":
						if _, err := assembleNarinfo(store, check.index); err != nil {
							proxy.log.Error("checking narinfo", zap.Error(err), zap.String("path", check.path))
							deadIndices.Store(check.path, yes)
						}
					}
				}
			}
		}(i)
	}

	walkIndicesErr := filepath.Walk(indices.Path, func(path string, info fs.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}

		isOld := info.ModTime().Before(ignoreBeforeTime)

		ext := filepath.Ext(path)
		isNar := ext == ".nar"
		isNarinfo := ext == ".narinfo"

		if !(isNar || isNarinfo || isOld) {
			return nil
		}

		name := path[len(indices.Path):]

		index, err := indices.GetIndex(name)
		if err != nil {
			return errors.WithMessagef(err, "while getting index %s", name)
		}

		integrity <- integrityCheck{path: path, index: index}

		inflatedSize += index.Length()
		indicesCount++

		if len(index.Chunks) == 0 {
			proxy.log.Debug("index chunks are empty", zap.String("path", path))
			deadIndices.Store(path, yes)
		} else {
			for _, indexChunk := range index.Chunks {
				if lru.IsDead(indexChunk.ID) {
					proxy.log.Debug("some chunks are dead", zap.String("path", path))
					deadIndices.Store(path, yes)
					break
				}
			}
		}

		return nil
	})

	wg.Wait()
	close(integrity)

	metricIndexCount.Set(indicesCount)
	metricIndexWalk.Add(uint64(time.Since(walkIndicesStart).Milliseconds()))
	metricInflated.Set(inflatedSize)

	if walkIndicesErr != nil {
		proxy.log.Error("While walking index", zap.Error(walkIndicesErr))
		return
	}
	deadIndexCount := uint64(0)
	// time.Sleep(10 * time.Minute)
	deadIndices.Range(func(key, value interface{}) bool {
		path := key.(string)
		proxy.log.Debug("moving index to trash", zap.String("path", path))
		_ = os.Remove(path)
		deadIndexCount++
		return true
	})

	metricIndexGcCount.Add(deadIndexCount)

	// we don't use store.Prune because it does another filepath.Walk and no
	// added benefit for us.

	for id := range lru.Dead() {
		if err := store.RemoveChunk(id); err != nil {
			proxy.log.Error("Removing chunk", zap.Error(err), zap.String("id", id.String()))
		}
	}

	proxy.log.Debug(
		"GC stats",
		zap.Uint64("live_bytes", lru.liveSize),
		zap.Uint64("live_max_bytes", lru.liveSizeMax),
		zap.Int("live_chunk_count", len(lru.live)),
		zap.Uint64("dead_bytes", lru.deadSize),
		zap.Int("dead_chunk_count", len(lru.dead)),
		zap.Uint64("dead_index_count", deadIndexCount),
		zap.Duration("walk_indices_time", time.Since(walkIndicesStart)),
	)
}
