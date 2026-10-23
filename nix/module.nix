# services.tidewaiter, and `tidewaiter` on every systemd service.
#
# Takes the flake's own packages, so a configuration that imports
# nixosModules.default runs the Tidewaiter it was built from by default.
{ packages }:
{
  config,
  lib,
  pkgs,
  ...
}:

let
  inherit (lib)
    attrNames
    concatStringsSep
    escapeShellArgs
    filterAttrs
    mapAttrs'
    mapAttrsToList
    mkEnableOption
    mkIf
    mkMerge
    mkOption
    nameValuePair
    optional
    optionals
    types
    ;

  cfg = config.services.tidewaiter;
  hasDocker = builtins.elem "docker" cfg.sources;

  # The services that opted in, by their attribute name.
  managed = filterAttrs (_: service: service.tidewaiter.desired != null) config.systemd.services;
  unitNames = map (name: "${name}.service") (attrNames managed);

  # Where the module declares them, and where both the daemon and prepare read.
  unitsDir = "/etc/tidewaiter/units";

  # One file per unit, the shape src/units.ts reads. Interpolating `desired`
  # puts it in the system closure, so the version a unit is asked to move to
  # was built (or substituted) by the switch that asked, and is kept from the
  # garbage collector by the system generation.
  declaration =
    name: service:
    let
      t = service.tidewaiter;
    in
    builtins.toJSON {
      unit = "${name}.service";
      desired = "${t.desired}";
      inherit (t)
        profile
        ports
        detector
        health
        idleSamples
        healthTimeout
        keepGenerations
        bindClosure
        ;
    };
  declarations = lib.mapAttrs declaration managed;

  exe = lib.getExe cfg.package;

  # A port in the label's grammar: `19132/udp`, `25565/tcp`, or a bare
  # `25565` for both. Checked here so that a typo fails the evaluation rather
  # than turning up as a warning in the journal.
  port = types.strMatching "[0-9]{1,5}(/(tcp|udp))?";
in
{
  options.services.tidewaiter = {
    enable = mkEnableOption "Tidewaiter, which updates systemd units (and Docker containers) only when they are idle";

    package = mkOption {
      type = types.package;
      default = packages.${pkgs.stdenv.hostPlatform.system}.default;
      defaultText = lib.literalMD "this flake's package";
      description = "The Tidewaiter to run, and to prepare units with.";
    };

    sources = mkOption {
      type = types.nonEmptyListOf (
        types.enum [
          "systemd"
          "docker"
        ]
      );
      default = [ "systemd" ];
      example = [
        "systemd"
        "docker"
      ];
      description = ''
        What to keep up to date: systemd units that set
        `systemd.services.<name>.tidewaiter.desired`, Docker containers
        labelled `tidewaiter.autoupdate`, or both. Run one Tidewaiter per host
        with every source it needs: one daemon is what keeps two swaps from
        ever overlapping, and holds one shutdown lock for both.
      '';
    };

    interval = mkOption {
      type = types.ints.positive;
      default = 60;
      description = ''
        Seconds between update passes. Shorter than the image's 300 because a
        pass over units costs no registry calls.
      '';
    };

    activityInterval = mkOption {
      type = types.ints.positive;
      default = 15;
      description = ''
        Seconds between the activity checks that keep the shutdown lock
        current between passes, so a player who joins just after a pass holds
        off a reboot within seconds.
      '';
    };

    inhibit = mkOption {
      type = types.bool;
      default = true;
      description = ''
        Hold a logind block lock on shutdown and sleep while anything managed
        is in use, or being swapped. Since systemd 257 a block lock also
        refuses root, so `system.autoUpgrade` with `allowReboot` cannot reboot
        under players: its `shutdown -r` is refused, the new generation stays
        installed for the next boot, and the next run tries again. A person can
        still override it with `systemctl reboot --check-inhibitors=no`.
      '';
    };

    dryRun = mkOption {
      type = types.bool;
      default = false;
      description = "Report what would change without changing anything.";
    };

    dockerSocket = mkOption {
      type = types.str;
      default = "/run/docker.sock";
      description = "The Docker socket, for the docker source.";
    };

    label = mkOption {
      type = types.str;
      default = "tidewaiter.autoupdate";
      description = "The label that opts a container in, for the docker source.";
    };

    extraArgs = mkOption {
      type = types.listOf types.str;
      default = [ ];
      example = [
        "--activity-interval"
        "5"
      ];
      description = "Further command line arguments, before the command.";
    };
  };

  # Extends the submodule every systemd.services.<name> already is, the way
  # nixpkgs' confinement module adds `confinement`: declaring the option again
  # merges these into it. Declared whether or not Tidewaiter is enabled here,
  # so a module such as Chandlery's can declare its units unconditionally.
  options.systemd.services = mkOption {
    type = types.attrsOf (
      types.submodule (
        { name, config, ... }:
        {
          options.tidewaiter = {
            desired = mkOption {
              type = types.nullOr types.package;
              default = null;
              example = lib.literalExpression "pkgs.minecraft-server";
              description = ''
                The version this service's profile should point at. Setting it
                opts the service in: a switch no longer restarts it
                (`restartIfChanged = false`); Tidewaiter moves `profile` to
                this path and restarts the service itself, once nobody is
                using it, and moves it back if the new version is unhealthy.
                The service's ExecStart must run from `profile`.
              '';
            };

            profile = mkOption {
              type = types.strMatching "/nix/var/nix/profiles/.+";
              default = "/nix/var/nix/profiles/tidewaiter/${name}";
              defaultText = lib.literalExpression ''"/nix/var/nix/profiles/tidewaiter/<name>"'';
              description = ''
                The Nix profile the service's ExecStart runs from, such as
                `''${profile}/bin/server`. Created on `desired` the first time
                the service is declared, before it starts.
              '';
            };

            ports = mkOption {
              type = types.listOf port;
              default = [ ];
              example = [
                "19132/udp"
                "25565"
              ];
              description = ''
                The ports whose flows mean someone is using the service, and
                that its health checks look at: `port/tcp`, `port/udp`, or a
                bare port for both.
              '';
            };

            detector = mkOption {
              type = types.enum [
                "conntrack"
                "tcp"
                "none"
              ];
              default = "conntrack";
              description = ''
                How to tell whether the service is in use. `conntrack` sees
                flows to `ports` in the host's connection table, UDP included;
                `tcp` counts established TCP connections; `none` always reads
                as idle.
              '';
            };

            health = mkOption {
              type = types.nonEmptyListOf (
                types.enum [
                  "systemd"
                  "port-connect"
                  "port-bound"
                  "uptime"
                ]
              );
              default = [
                "systemd"
                "port-connect"
                "port-bound"
                "uptime"
              ];
              description = ''
                The checks an update must pass, run together; any one failing
                rolls it back. `systemd`: the restart job finished and the
                service stayed up on that invocation. `port-connect`: TCP
                `ports` accept on localhost. `port-bound`: a socket in the
                service's own cgroup is on one of `ports`. `uptime`: it stays
                up, and by then has bound a port.
              '';
            };

            idleSamples = mkOption {
              type = types.ints.positive;
              default = 3;
              description = "Consecutive idle passes before the service is updated.";
            };

            healthTimeout = mkOption {
              type = types.ints.positive;
              default = 120;
              description = "Seconds to wait for the new version to be healthy.";
            };

            keepGenerations = mkOption {
              type = types.ints.unsigned;
              default = 1;
              description = "Earlier generations of `profile` kept after a healthy update, for a manual rollback.";
            };

            bindClosure = mkOption {
              type = types.bool;
              default = config.confinement.enable;
              defaultText = lib.literalExpression "config.systemd.services.<name>.confinement.enable";
              description = ''
                Bind what `profile` points at, and its closure, into the
                service's confined root, with a runtime drop-in rewritten
                before every restart. A confined service needs it: its root
                holds only the closure of its own definition, which names the
                profile but no version.
              '';
            };
          };

          config = mkIf (config.tidewaiter.desired != null) {
            restartIfChanged = false;
            # Its profile, and for a confined service its closure drop-in, have
            # to exist before it starts. Wants, not Requires: a restart of
            # prepare on a switch must not restart the service with it.
            wants = [ "tidewaiter-prepare.service" ];
            after = [ "tidewaiter-prepare.service" ];
          };
        }
      )
    );
  };

  config = mkMerge [
    {
      environment.etc = mapAttrs' (
        name: text: nameValuePair "tidewaiter/units/${name}.service.json" { inherit text; }
      ) declarations;

      # Before any managed service starts, at boot and on every switch that
      # changes a declaration: create a newly declared service's profile, and
      # write the closure drop-ins, which live in /run and so are gone after
      # every reboot. A unit rather than an activation script, because
      # activation at boot runs before the Nix daemon - and, in a VM test,
      # before the store's paths are even registered.
      #
      # Always defined, and only enabled when something opted in: whether a
      # systemd service exists cannot depend on the other services, or the
      # module system would have to know them all to know them all.
      systemd.services.tidewaiter-prepare = {
        enable = managed != { };
        description = "Prepare the services Tidewaiter manages";
        documentation = [ "https://github.com/danielbodart/tidewaiter" ];
        wantedBy = [ "multi-user.target" ];
        wants = [ "nix-daemon.socket" ];
        after = [ "nix-daemon.socket" ];
        before = unitNames;
        restartTriggers = mapAttrsToList (_: text: text) declarations;
        path = [ config.nix.package ];
        environment.NIX_REMOTE = "daemon";
        serviceConfig = {
          Type = "oneshot";
          RemainAfterExit = true;
          ExecStart = escapeShellArgs [
            exe
            "--units-dir"
            unitsDir
            "prepare"
          ];
        };
      };

      assertions = mapAttrsToList (name: service: {
        assertion = service.tidewaiter.detector == "none" || service.tidewaiter.ports != [ ];
        message = ''
          systemd.services.${name}.tidewaiter.ports is empty, so the ${service.tidewaiter.detector}
          detector cannot tell whether it is in use and Tidewaiter would wait forever.
          Declare its ports, or set detector = "none".
        '';
      }) managed;

      warnings =
        optional (managed != { } && !cfg.enable) ''
          ${concatStringsSep ", " unitNames} set tidewaiter.desired, but services.tidewaiter.enable is false:
          their profiles are created, but never updated.
        ''
        ++ optional (managed != { } && cfg.enable && !builtins.elem "systemd" cfg.sources) ''
          ${concatStringsSep ", " unitNames} set tidewaiter.desired, but services.tidewaiter.sources has no "systemd".
        '';
    }

    (mkIf cfg.enable {
      warnings = optional (cfg.inhibit && lib.versionOlder config.systemd.package.version "257") ''
        services.tidewaiter.inhibit: systemd ${config.systemd.package.version} lets root ignore block
        inhibitors, so an automatic reboot will not wait for players. It needs systemd 257 or later.
      '';

      systemd.services.tidewaiter = {
        description = "Tidewaiter, updating services only when they are idle";
        documentation = [ "https://github.com/danielbodart/tidewaiter" ];
        wantedBy = [ "multi-user.target" ];
        wants = [ "nix-daemon.socket" ];
        after = [
          "network.target"
          "nix-daemon.socket"
        ]
        ++ optional hasDocker "docker.service";
        # nix-env and nix-store as the system has them, before the package's own.
        path = [ config.nix.package ];

        environment = {
          # Through the daemon, so writing a profile takes no privilege over
          # the store itself.
          NIX_REMOTE = "daemon";
          # nix-env wants a home; root's is out of sight (ProtectHome).
          HOME = "/var/lib/tidewaiter";
        };

        serviceConfig = {
          ExecStart = escapeShellArgs (
            [
              exe
              "--source"
              (concatStringsSep "," cfg.sources)
              "--interval"
              (toString cfg.interval)
              "--activity-interval"
              (toString cfg.activityInterval)
              (if cfg.inhibit then "--inhibit" else "--no-inhibit")
              "--units-dir"
              unitsDir
            ]
            ++ optionals hasDocker [
              "--docker-socket"
              cfg.dockerSocket
              "--label"
              cfg.label
            ]
            ++ optional cfg.dryRun "--dry-run"
            ++ cfg.extraArgs
            ++ [ "run" ]
          );
          Restart = "on-failure";
          RestartSec = 5;
          StateDirectory = "tidewaiter";

          # A pass in flight is finished before Tidewaiter exits, and a swap
          # can take a game server's whole stop (Chandlery allows 5 minutes),
          # its start and two health gates. Killing it halfway would leave a
          # profile moved with no gate run. Mixed, so the shutdown lock's
          # child is not signalled until Tidewaiter itself has gone.
          TimeoutStopSec = "15min";
          KillMode = "mixed";

          # Root, because it restarts other services, writes system profiles
          # and reads the host's connection table - and otherwise confined as
          # far as that allows. Everything it writes is root's, so uid 0
          # without CAP_DAC_OVERRIDE is enough; CAP_NET_ADMIN is for
          # conntrack. systemd and logind recognise root by uid on the bus.
          CapabilityBoundingSet = [ "CAP_NET_ADMIN" ];
          NoNewPrivileges = true;
          ProtectSystem = "strict";
          ReadWritePaths = [
            "/nix/var/nix/profiles"
            # The closure drop-ins.
            "/run/systemd/system"
          ];
          ProtectHome = true;
          PrivateTmp = true;
          PrivateDevices = true;
          ProtectKernelTunables = true;
          ProtectKernelModules = true;
          ProtectKernelLogs = true;
          # Read-only: ss reads cgroupfs to name a socket's cgroup.
          ProtectControlGroups = true;
          ProtectClock = true;
          ProtectHostname = true;
          RestrictNamespaces = true;
          RestrictRealtime = true;
          RestrictSUIDSGID = true;
          LockPersonality = true;
          SystemCallArchitectures = "native";
          SystemCallFilter = [
            "@system-service"
            "~@privileged"
          ];
          # The drop-ins are for systemd, but an operator should be able to
          # read them too.
          UMask = "0022";
          # The bus and the Nix daemon; conntrack and sock_diag; port-connect.
          RestrictAddressFamilies = [
            "AF_UNIX"
            "AF_NETLINK"
            "AF_INET"
            "AF_INET6"
          ];
          # Units only ever probe localhost; Docker's registry is the internet.
          IPAddressAllow = mkIf (!hasDocker) [ "localhost" ];
          IPAddressDeny = mkIf (!hasDocker) [ "any" ];
          SupplementaryGroups = optional hasDocker "docker";
          # Not MemoryDenyWriteExecute: Bun's JIT needs writable code.
          # Not PrivateNetwork: conntrack and port-connect are the host's.
          # Not PrivateUsers: it must be real root to systemd and the Nix daemon.
          # Not ProtectProc: the tcp detector reads /proc/<MainPID>/net.
        };
      };
    })
  ];
}
