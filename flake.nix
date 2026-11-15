{
  description = "Update Docker containers and systemd units only when they are idle";

  # The release nix-config builds every host from. The package needs little
  # from it (the release's binary, wrapped with nix on PATH); the module check
  # and the VM test need the rest.
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-26.05";

  outputs =
    { self, nixpkgs }:
    let
      inherit (nixpkgs) lib;
      systems = [
        "x86_64-linux"
        "aarch64-linux"
      ];
      forAllSystems = f: lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});

      # THE PACKAGE IS THE RELEASE BINARY, not a build from source (Dan,
      # 2026-11-15): every binary is built once, by mise's Go in CI's dist
      # job, tested in every channel, signed and published, and building it
      # again here only made a second binary that was not the one released.
      # So a release's tag is a commit of its own, a child of the released
      # master commit, that adds nix/release.json: the version, and for each
      # system the binary's URL and hash (./run release-json writes it;
      # ci.yml's release job commits and tags it). A flake fetched as
      # github:danielbodart/tidewaiter/v<V> sees its commit but not its tag,
      # which is why the version is in the file.
      #
      # On master there is no such file and no package: pin a release tag.
      # Absent rather than an error, so `nix flake check` still works there.
      releaseFile = ./nix/release.json;
      release = if builtins.pathExists releaseFile then lib.importJSON releaseFile else null;

      # The release's static binary, byte for byte, as bin/.tidewaiter-wrapped,
      # behind a wrapper that puts nix on PATH. Not stripped or patched: a
      # static Go binary needs neither, and either would make it differ from
      # SHA256SUMS. The rest of the fixup stays, for the wrapper's shebang.
      #
      # What it runs: nix-env and nix-store, for the systemd source's profiles
      # and closures. Nothing else - the conntrack table and the sockets are
      # asked of the kernel over netlink. Suffixed, not prefixed, so the
      # host's own come first: on NixOS the module puts config.nix.package on
      # PATH. systemd itself is only ever spoken to over the bus.
      mkTidewaiter =
        pkgs:
        { version, binary }:
        pkgs.stdenvNoCC.mkDerivation {
          pname = "tidewaiter";
          inherit version;
          src = binary;
          nativeBuildInputs = [ pkgs.makeWrapper ];
          dontUnpack = true;
          dontConfigure = true;
          dontBuild = true;
          installPhase = ''
            runHook preInstall
            install -Dm0755 $src $out/bin/tidewaiter
            wrapProgram $out/bin/tidewaiter --suffix PATH : ${lib.makeBinPath [ pkgs.nix ]}
            runHook postInstall
          '';
          dontStrip = true;
          dontPatchELF = true;
          meta = {
            description = "Update Docker containers and systemd units only when they are idle, health-gate the swap, roll back on failure";
            homepage = "https://github.com/danielbodart/tidewaiter";
            license = lib.licenses.asl20;
            mainProgram = "tidewaiter";
            # x86_64, aarch64 and armv7l (a 32-bit Pi), as release.json lists them.
            platforms = if release == null then [ ] else builtins.attrNames release.assets;
            sourceProvenance = [ lib.sourceTypes.binaryNativeCode ];
          };
        };

      # The release's package for each system it has a binary for.
      releasePackages = lib.mapAttrs (
        system: asset:
        let
          pkgs = nixpkgs.legacyPackages.${system};
          tidewaiter = mkTidewaiter pkgs {
            inherit (release) version;
            binary = pkgs.fetchurl { inherit (asset) url hash; };
          };
        in
        {
          inherit tidewaiter;
          default = tidewaiter;
        }
      ) release.assets;
    in
    {
      packages = if release == null then { } else releasePackages;

      nixosModules.default = import ./nix/module.nix { inherit (self) packages; };

      # The tests, gofmt and vet are mise run check's. The module check
      # renders a configuration with a stand-in package, so it runs on master
      # too; the VM test runs the release's package, so it exists where
      # nix/release.json does: on a release tag, and in CI's nix job, which
      # writes one for the binaries its own run built (./run nix-local-release).
      checks = forAllSystems (
        pkgs:
        {
          module = import ./nix/module-check.nix { inherit pkgs self; };
        }
        // lib.optionalAttrs (self.packages ? ${pkgs.stdenv.hostPlatform.system}) {
          vm = import ./nix/vm-test.nix { inherit pkgs self; };
        }
      );

      formatter = forAllSystems (pkgs: pkgs.nixfmt);
    };
}
