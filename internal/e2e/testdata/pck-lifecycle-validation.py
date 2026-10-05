import hashlib
import os
import pathlib
import signal
import subprocess
import tempfile
import time

binary = os.environ["BACKPACK_PCK_BINARY"]
work = pathlib.Path(tempfile.mkdtemp(prefix="pck-engine-validation-"))
config = work / "incident.toml"
delay = work / "delay-deletion"
real_iptables = subprocess.check_output(["which", "iptables"], text=True).strip()
wrapper = work / "iptables"
wrapper.write_text(f'#!/bin/sh\ncase "$*" in *"-D "*) if [ -f "{delay}" ]; then sleep 2; fi;; esac\nexec "{real_iptables}" "$@"\n')
wrapper.chmod(0o755)
environment = dict(os.environ, PATH=f"{work}:{os.environ['PATH']}")
tokens = [hashlib.sha256(f"engine-token-{i}".encode()).hexdigest() for i in range(3)]


def tag(token):
    return "backpack-pck-" + hashlib.sha256(("backpack-pck-v1:"+token).encode()).hexdigest()[:8] + "-"


def rules(token):
    return sum(subprocess.check_output([real_iptables, "-t", table, "-S"], text=True).count(tag(token)) for table in ("filter", "raw"))


def wait_for(predicate, description, timeout=20):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        if engine.poll() is not None:
            raise AssertionError(f"engine exited during {description}: {engine.returncode}")
        time.sleep(0.15)
    raise AssertionError(f"timeout waiting for {description}")


def write_config(transport, token):
    config.write_text(f'[server]\nbind_addr = "192.0.2.1:53655"\ntransport = "{transport}"\ntoken = "{token}"\nports = []\nskip_optz = true\npck_interface = "bp-test"\npck_gateway_mac = "02:00:00:00:00:02"\n')


def start():
    return subprocess.Popen([binary, "-c", str(config)], env=environment, stdout=log, stderr=subprocess.STDOUT)


log = open(work / "engine.log", "w")
engine = None
try:
    # An orphan whose old token is absent from the new config is still found.
    subprocess.run([real_iptables, "-I", "OUTPUT", "-p", "tcp", "--sport", "49000:49127", "--tcp-flags", "RST", "RST", "-m", "comment", "--comment", tag(tokens[2])+"49000:49127", "-j", "DROP"], check=True)
    write_config("pck", tokens[0])
    engine = start()
    wait_for(lambda: rules(tokens[0]) == 3 and rules(tokens[2]) == 0, "PCK startup and replaced-token cleanup")
    original_pid = engine.pid
    write_config("tcpmux", tokens[0])
    wait_for(lambda: rules(tokens[0]) == 0, "PCK to TCPMUX reload")
    assert engine.pid == original_pid and engine.poll() is None
    write_config("pck", tokens[1])
    wait_for(lambda: rules(tokens[1]) == 3, "new-token PCK reload")
    assert rules(tokens[0]) == 0
    engine.kill()
    engine.wait(timeout=5)
    assert rules(tokens[1]) == 3, "SIGKILL did not leave a valid orphan fixture"
    write_config("tcpmux", tokens[0])
    engine = start()
    wait_for(lambda: rules(tokens[1]) == 0, "TCPMUX recovery after crashed PCK with a replaced token")
    write_config("pck", tokens[0])
    wait_for(lambda: rules(tokens[0]) == 3, "PCK before delayed teardown")
    # Teardown intentionally takes longer than the old main's one-second exit.
    delay.touch()
    engine.send_signal(signal.SIGTERM)
    engine.wait(timeout=20)
    assert engine.returncode == 0
    assert rules(tokens[0]) == 0, "process exited before delayed guard teardown finished"
    print("PASS: live PCK/TCPMUX reload, replaced token, SIGKILL recovery and delayed graceful teardown")
finally:
    if engine is not None and engine.poll() is None:
        engine.kill()
        engine.wait(timeout=5)
    log.close()
    print((work / "engine.log").read_text()[-6000:])
