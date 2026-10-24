{
  lib,
  buildGo127Module,
  makeWrapper,
  nix,
  version,
  src,
}:

# The same static build as ./run, by nixpkgs' Go, and the check phase runs the
# tests on the way - the ones that need a system bus, Docker, cgroupfs or
# netlink permission skip themselves in the sandbox. buildGo127Module rather
# than buildGoModule, which is still Go 1.26 in the pinned nixpkgs and would
# refuse go.mod's `go 1.27`.
buildGo127Module {
  pname = "tidewaiter";
  inherit version src;

  # godbus, gokit, gokit/dbusx and the golang.org/x/sys godbus needs, as
  # `go mod vendor` lays them out. A go.sum change needs it recomputed: build once with
  # lib.fakeHash and take the hash Nix reports.
  vendorHash = "sha256-AnUovKOpEnvpLb/qKdAoGIB9VB7VxrXMovBCNlVIw3A=";

  nativeBuildInputs = [ makeWrapper ];

  env.CGO_ENABLED = 0;
  ldflags = [
    "-s"
    "-w"
    "-X github.com/danielbodart/tidewaiter/internal/version.Version=${version}"
  ];

  # What it runs: nix-env and nix-store, for the systemd source's profiles and
  # closures. Nothing else - the conntrack table and the sockets are asked of
  # the kernel over netlink, where conntrack and ss used to be on this PATH.
  # Suffixed, not prefixed, so the host's own come first: on NixOS the module
  # puts config.nix.package on PATH. systemd itself is only ever spoken to over
  # the bus.
  postInstall = ''
    wrapProgram $out/bin/tidewaiter --suffix PATH : ${lib.makeBinPath [ nix ]}
  '';

  meta = {
    description = "Update Docker containers and systemd units only when they are idle, health-gate the swap, roll back on failure";
    homepage = "https://github.com/danielbodart/tidewaiter";
    license = lib.licenses.asl20;
    platforms = [
      "x86_64-linux"
      "aarch64-linux"
      "armv7l-linux"
    ];
    mainProgram = "tidewaiter";
  };
}
