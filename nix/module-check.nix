# Evaluates a configuration that uses the module and checks what it renders,
# without booting anything - the part of the VM test that needs no KVM.
{ pkgs, self }:

let
  game = pkgs.writeShellScriptBin "game" "exec sleep infinity";

  system = pkgs.nixos {
    imports = [ self.nixosModules.default ];
    boot.loader.grub.enable = false;
    fileSystems."/" = {
      device = "none";
      fsType = "tmpfs";
    };
    system.stateVersion = "25.11";

    services.tidewaiter.enable = true;

    # Confined, so bindClosure defaults on.
    systemd.services.game = {
      wantedBy = [ "multi-user.target" ];
      confinement.enable = true;
      serviceConfig.ExecStart = "/nix/var/nix/profiles/games/game/bin/game";
      tidewaiter = {
        desired = game;
        profile = "/nix/var/nix/profiles/games/game";
        ports = [
          "19132/udp"
          "25565"
        ];
        idleSamples = 2;
      };
    };

    # Not opted in: untouched.
    systemd.services.plain = {
      wantedBy = [ "multi-user.target" ];
      serviceConfig.ExecStart = "${pkgs.coreutils}/bin/sleep infinity";
    };
  };

  # The same, with nothing opted in and the daemon off: the module imported
  # costs nothing.
  bare = pkgs.nixos {
    imports = [ self.nixosModules.default ];
    boot.loader.grub.enable = false;
    fileSystems."/" = {
      device = "none";
      fsType = "tmpfs";
    };
    system.stateVersion = "25.11";
  };

  inherit (system.config) systemd environment;
in
pkgs.runCommand "tidewaiter-module-check"
  {
    declaration = environment.etc."tidewaiter/units/game.service.json".text;
    gameUnit = systemd.units."game.service".text;
    plainRestarts = builtins.toJSON systemd.services.plain.restartIfChanged;
    gameRestarts = builtins.toJSON systemd.services.game.restartIfChanged;
    daemon = systemd.units."tidewaiter.service".text;
    prepare = systemd.units."tidewaiter-prepare.service".text;
    prepareEnabled = builtins.toJSON systemd.units."tidewaiter-prepare.service".enable;
    bareEnabled = builtins.toJSON bare.config.systemd.units."tidewaiter-prepare.service".enable;
    bareDaemon = builtins.toJSON (bare.config.systemd.units ? "tidewaiter.service");
    bareEtc = builtins.toJSON (
      builtins.any (name: pkgs.lib.hasPrefix "tidewaiter" name) (
        builtins.attrNames bare.config.environment.etc
      )
    );
    gamePath = "${game}";
    nativeBuildInputs = [ pkgs.jq ];
  }
  ''
    fail() { echo "FAIL: $*"; exit 1; }

    # The declaration: the whole policy, desired as its store path.
    jq -e --arg game "$gamePath" '
      .unit == "game.service" and .desired == $game
      and .profile == "/nix/var/nix/profiles/games/game"
      and .ports == ["19132/udp", "25565"] and .detector == "conntrack"
      and .health == ["systemd", "port-connect", "port-bound", "uptime"]
      and .idleSamples == 2 and .healthTimeout == 120 and .keepGenerations == 1
      and .bindClosure == true
    ' <<<"$declaration" >/dev/null || fail "declaration: $declaration"

    # A switch never restarts an opted-in service; the rest are as they were.
    [[ $gameRestarts == false ]] || fail "game.service restartIfChanged is $gameRestarts"
    [[ $plainRestarts == true ]] || fail "plain.service restartIfChanged is $plainRestarts"
    grep -qx 'X-RestartIfChanged=false' <<<"$gameUnit" || fail "game.service has no X-RestartIfChanged=false"

    # Prepared before it starts, wanted rather than required.
    grep -q '^After=.*tidewaiter-prepare.service' <<<"$gameUnit" || fail "game.service is not after prepare"
    grep -q '^Wants=.*tidewaiter-prepare.service' <<<"$gameUnit" || fail "game.service does not want prepare"
    if grep -q '^Requires=.*tidewaiter-prepare' <<<"$gameUnit"; then fail "game.service requires prepare"; fi

    [[ $prepareEnabled == true ]] || fail "prepare is not enabled"
    grep -q 'prepare$' <<<"$(grep '^ExecStart=' <<<"$prepare")" || fail "prepare does not run prepare"
    grep -q -- '--units-dir /etc/tidewaiter/units' <<<"$prepare" || fail "prepare reads the wrong directory"
    grep -qx 'Type=oneshot' <<<"$prepare" || fail "prepare is not a oneshot"
    grep -qx 'RemainAfterExit=true' <<<"$prepare" || fail "prepare does not remain"
    # Re-run on any switch that changes a declaration.
    grep -q '^X-Restart-Triggers=' <<<"$prepare" || fail "prepare has no restart triggers"

    # The daemon.
    grep -q -- '--source systemd' <<<"$daemon" || fail "sources not passed"
    grep -q -- '--interval 60' <<<"$daemon" || fail "interval not passed"
    grep -q -- '--activity-interval 15' <<<"$daemon" || fail "activity interval not passed"
    grep -q -- '--inhibit' <<<"$daemon" || fail "inhibit not passed"
    grep -q -- '--units-dir /etc/tidewaiter/units' <<<"$daemon" || fail "units dir not passed"
    if grep -q -- '--docker-socket' <<<"$daemon"; then fail "docker socket passed without docker"; fi
    grep -qx 'StateDirectory=tidewaiter' <<<"$daemon" || fail "no state directory"
    grep -qx 'Environment="NIX_REMOTE=daemon"' <<<"$daemon" || fail "not through the Nix daemon"
    grep -qx 'CapabilityBoundingSet=CAP_NET_ADMIN' <<<"$daemon" || fail "capabilities not bounded"
    grep -qx 'ProtectSystem=strict' <<<"$daemon" || fail "system not protected"
    grep -qx 'ReadWritePaths=/nix/var/nix/profiles' <<<"$daemon" || fail "profiles not writable"
    grep -qx 'ReadWritePaths=/run/systemd/system' <<<"$daemon" || fail "drop-ins not writable"
    grep -qx 'IPAddressDeny=any' <<<"$daemon" || fail "IP not restricted for systemd only"

    # Imported with nothing opted in and the daemon off: nothing runs, nothing in /etc.
    [[ $bareEnabled == false ]] || fail "prepare enabled with nothing to prepare"
    [[ $bareDaemon == false ]] || fail "daemon defined while disabled"
    [[ $bareEtc == false ]] || fail "/etc/tidewaiter written with nothing declared"

    touch $out
  ''
