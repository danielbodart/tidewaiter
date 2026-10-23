{
  lib,
  stdenvNoCC,
  bun,
  makeWrapper,
  conntrack-tools,
  iproute2,
  nix,
  systemd,
  version,
  src,
}:

# The same `bun build --compile` as run.ts's build. Tidewaiter has no runtime
# dependencies, so there is nothing to fetch and it builds in the sandbox from
# the source alone; typescript and @types/bun are only for tsc, which this
# does not run.
stdenvNoCC.mkDerivation {
  pname = "tidewaiter";
  inherit version src;

  nativeBuildInputs = [
    bun
    makeWrapper
  ];

  buildPhase = ''
    runHook preBuild
    export HOME=$TMPDIR
    bun build --compile --minify --sourcemap \
      --define 'TIDEWAITER_VERSION="${version}"' \
      src/main.ts --outfile tidewaiter
    runHook postBuild
  '';

  # The tests that need a Nix daemon or a system bus skip themselves here.
  doCheck = true;
  checkPhase = ''
    runHook preCheck
    bun test
    runHook postCheck
  '';

  # What it runs: conntrack (the activity detector), ss (sockets by cgroup),
  # systemd-inhibit (the shutdown lock), nix-env and nix-store (profiles and
  # closures). Suffixed, not prefixed, so the host's own come first: on NixOS
  # the module puts config.nix.package and the running systemd on PATH.
  installPhase = ''
    runHook preInstall
    install -Dm755 tidewaiter $out/libexec/tidewaiter
    makeWrapper $out/libexec/tidewaiter $out/bin/tidewaiter \
      --suffix PATH : ${
        lib.makeBinPath [
          conntrack-tools
          iproute2
          systemd
          nix
        ]
      }
    runHook postInstall
  '';

  # A compiled Bun binary is Bun with the bundle appended to it. Stripping it,
  # or patchelf rewriting it, loses the bundle and leaves plain Bun behind.
  dontStrip = true;
  dontPatchELF = true;

  meta = {
    description = "Update Docker containers and systemd units only when they are idle, health-gate the swap, roll back on failure";
    homepage = "https://github.com/danielbodart/tidewaiter";
    license = lib.licenses.asl20;
    platforms = [
      "x86_64-linux"
      "aarch64-linux"
    ];
    mainProgram = "tidewaiter";
  };
}
