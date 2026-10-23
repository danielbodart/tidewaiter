# One machine running a small UDP and TCP echo service from a Nix profile,
# confined as Chandlery's game servers are, and Tidewaiter keeping it on the
# version its configuration asks for. Specialisations that only change
# `tidewaiter.desired` stand in for a deploy: v2, and a bad build that never
# comes up.
#
# What it shows: a switch does not restart the service; Tidewaiter waits while
# a player is connected, holding a shutdown lock that refuses a reboot; it swaps
# once the player has gone; and it rolls a bad version back, and keeps it out
# across a restart of itself.
{ pkgs, self }:

let
  inherit (pkgs) lib;

  profile = "/nix/var/nix/profiles/tidewaiter/echo-game";

  # Answers its version to anything on 7777, UDP and TCP, and tells systemd it
  # is up once both are bound. The bad build exits after two seconds without
  # binding anything or saying it is ready.
  game =
    version:
    pkgs.writeScriptBin "echo-game" ''
      #!${pkgs.python3}/bin/python3
      import os, socket, sys, threading, time

      VERSION = "${version}"
      if VERSION == "bad":
          time.sleep(2)
          sys.exit(1)

      udp = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
      udp.bind(("0.0.0.0", 7777))
      tcp = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
      tcp.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
      tcp.bind(("0.0.0.0", 7777))
      tcp.listen()

      def serve_udp():
          while True:
              _, peer = udp.recvfrom(1024)
              udp.sendto(f"{VERSION}\n".encode(), peer)

      threading.Thread(target=serve_udp, daemon=True).start()

      address = os.environ.get("NOTIFY_SOCKET", "")
      if address:
          notify = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
          notify.sendto(b"READY=1", "\0" + address[1:] if address.startswith("@") else address)

      while True:
          connection, _ = tcp.accept()
          with connection:
              connection.sendall(f"{VERSION}\n".encode())
    '';

  v1 = game "v1";
  v2 = game "v2";
  bad = game "bad";

  # A player: a UDP datagram to the game every second, for as long as it runs.
  player = pkgs.writeScript "player" ''
    #!${pkgs.python3}/bin/python3
    import socket, time
    s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    s.settimeout(1)
    while True:
        s.sendto(b"hello", ("127.0.0.1", 7777))
        try:
            s.recv(64)
        except OSError:
            pass
        time.sleep(1)
  '';
in
pkgs.testers.runNixOSTest {
  name = "tidewaiter";

  nodes.machine = {
    imports = [ self.nixosModules.default ];

    environment.systemPackages = [ pkgs.socat ];

    # An idle UDP flow ages out of conntrack in seconds, not the default 30
    # (or 120 once it has seen a reply), so "the player left" happens within
    # the test. The module is loaded at boot so the sysctls exist to be set.
    boot.kernelModules = [ "nf_conntrack" ];
    boot.kernel.sysctl = {
      "net.netfilter.nf_conntrack_udp_timeout" = 5;
      "net.netfilter.nf_conntrack_udp_timeout_stream" = 5;
    };

    services.tidewaiter = {
      enable = true;
      interval = 2;
      activityInterval = 1;
    };

    systemd.services.echo-game = {
      wantedBy = [ "multi-user.target" ];
      # Only its own closure in its root, as Chandlery's servers have, which
      # is why Tidewaiter binds the profile's in (bindClosure defaults on).
      confinement.enable = true;
      serviceConfig = {
        Type = "notify";
        ExecStart = "${profile}/bin/echo-game";
        TimeoutStartSec = 10;
      };
      tidewaiter = {
        desired = v1;
        inherit profile;
        ports = [
          "7777/udp"
          "7777/tcp"
        ];
        idleSamples = 2;
        healthTimeout = 20;
      };
    };

    specialisation.v2.configuration.systemd.services.echo-game.tidewaiter.desired = lib.mkForce v2;
    specialisation.bad.configuration.systemd.services.echo-game.tidewaiter.desired = lib.mkForce bad;
  };

  testScript =
    { nodes, ... }:
    let
      toplevel = nodes.machine.system.build.toplevel;
    in
    ''
      import datetime as dt
      import json

      def target():
          return machine.succeed("readlink -f ${profile}").strip()

      def invocation():
          return machine.succeed("systemctl show -p InvocationID --value echo-game.service").strip()

      def answers(version):
          machine.wait_until_succeeds(f"echo | socat -T2 - TCP:127.0.0.1:7777 | grep -x {version}", timeout=dt.timedelta(seconds=60))
          machine.wait_until_succeeds(f"echo | socat -T2 - UDP:127.0.0.1:7777 | grep -x {version}", timeout=dt.timedelta(seconds=60))

      def switch(specialisation):
          machine.succeed(f"${toplevel}/specialisation/{specialisation}/bin/switch-to-configuration test")

      def locked():
          return "tidewaiter" in machine.succeed("systemd-inhibit --list --no-legend --no-pager")

      def wait_passes(n=5):
          # Passes are every 2s; give it n of them.
          machine.sleep(duration=dt.timedelta(seconds=2 * n))

      machine.wait_for_unit("multi-user.target")
      machine.wait_for_unit("echo-game.service")
      machine.wait_for_unit("tidewaiter.service")

      with subtest("boots on v1: profile created, closure bound, confined"):
          assert target() == "${v1}", target()
          answers("v1")
          machine.succeed("test -f /run/systemd/system/echo-game.service.d/50-tidewaiter-closure.conf")
          machine.succeed("systemctl show -p RootDirectory --value echo-game.service | grep -q /run/confinement")
          print(machine.succeed("cat /etc/tidewaiter/units/echo-game.service.json"))

      with subtest("a player holds the shutdown lock"):
          machine.succeed("systemd-run --unit=player ${player}")
          machine.wait_until_succeeds("systemd-inhibit --list --no-legend --no-pager | grep -q 'tidewaiter.*echo-game.service'", timeout=dt.timedelta(seconds=30))
          print(machine.succeed("systemd-inhibit --list --no-pager"))

      with subtest("a switch to v2 does not restart the game"):
          before = invocation()
          switch("v2")
          desired = json.loads(machine.succeed("cat /etc/tidewaiter/units/echo-game.service.json"))["desired"]
          assert desired == "${v2}", desired
          assert invocation() == before, "the switch restarted echo-game"

      with subtest("while the player is on, a reboot is refused"):
          status, output = machine.execute("shutdown -r +5 2>&1")
          print(output)
          if status == 0:
              machine.succeed("shutdown -c")
          assert status != 0, "shutdown -r was accepted while the lock was held"
          status, output = machine.execute("systemctl reboot 2>&1")
          print(output)
          assert status != 0, "systemctl reboot was accepted while the lock was held"

      with subtest("and the game is left alone"):
          wait_passes()
          assert target() == "${v1}", target()
          assert invocation() == before, "echo-game was restarted with a player on it"
          answers("v1")

      with subtest("once the player has gone, it moves to v2 and lets go of the lock"):
          machine.succeed("systemctl stop player")
          machine.wait_until_succeeds("readlink -f ${profile} | grep -qx ${v2}", timeout=dt.timedelta(seconds=120))
          answers("v2")
          assert invocation() != before, "echo-game was not restarted"
          machine.wait_until_succeeds("! systemd-inhibit --list --no-legend --no-pager | grep -q tidewaiter", timeout=dt.timedelta(seconds=30))
          machine.wait_until_succeeds("journalctl -u tidewaiter.service -o cat | grep -q 'updated to ${v2} and it is healthy'")

      with subtest("a bad version is rolled back, and pinned out"):
          switch("bad")
          machine.wait_until_succeeds("journalctl -u tidewaiter.service -o cat | grep -q 'rolled back to ${v2}'", timeout=dt.timedelta(seconds=120))
          assert target() == "${v2}", target()
          machine.wait_for_unit("echo-game.service")
          answers("v2")
          settled = invocation()
          wait_passes()
          assert invocation() == settled, "the bad version was tried again"
          state = json.loads(machine.succeed("cat /var/lib/tidewaiter/systemd.json"))
          assert state["echo-game.service"]["pinned"] == "${bad}", state
          generations = machine.succeed("nix-env --profile ${profile} --list-generations")
          print(generations)

      with subtest("the pin survives a restart of Tidewaiter"):
          machine.succeed("systemctl restart tidewaiter.service")
          machine.wait_for_unit("tidewaiter.service")
          wait_passes()
          assert invocation() == settled, "the bad version was tried again after a restart"
          assert target() == "${v2}", target()

      with subtest("how confined the daemon is"):
          print(machine.succeed("systemd-analyze security tidewaiter.service --no-pager"))
          print(machine.succeed("journalctl -u tidewaiter.service -u tidewaiter-prepare.service -o cat --no-pager"))
    '';
}
