{
  name,
  std,
  lib,
  actionLib,
  ...
} @ args: let
  startOf = of: of.value."${name}".start;
in {
  inputs.start = ''
    "${name}": start: {
      clone_url: string
      sha: string
      statuses_url?: string
    }
  '';

  output = {start}: let
    facts = start.value."${name}".start;
  in {
    success."${name}" = {
      ok = true;
      inherit (facts) clone_url sha;
    };
  };

  job = {start}: let
    facts = start.value."${name}".start;
  in
    std.chain args [
      actionLib.simpleJob

      {resources.memory = 1024 * 6;}

      (lib.optionalAttrs (facts ? statuses_url)
        (std.github.reportStatus facts.statuses_url))

      (std.git.clone facts)

      std.nix.develop

      (std.wrapScript "bash" (next: ''
        set -ex
        lint
        ${lib.escapeShellArgs next}
      ''))

      std.nix.build
    ];
}