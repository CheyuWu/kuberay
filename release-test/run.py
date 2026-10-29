# /// script
# requires-python = ">=3.10"
# dependencies = ["pyyaml"]
# ///
"""Run the units in release-test/manifest.yaml on the current kind cluster.

A step passes when its run and its check both exit 0; the first step that fails ends the unit. The
runner then saves evidence from the cluster, runs the unit's cleanup and writes
results/TAG/ID/result.json. Ctrl-C or SIGTERM still runs the cleanup but writes no result, so the
unit runs again next time.

Without IDs it resumes, skipping units that already have a result; with IDs it runs exactly those.
Steps see the tag as $KUBERAY_VERSION and the tag without the v as $CHART_VERSION.

examples:
  uv run release-test/run.py --kuberay-version v1.8.0-rc.0
  uv run release-test/run.py --kuberay-version v1.8.0-rc.0 raycluster-quick-start ray-cluster-sample
"""

import argparse
import json
import os
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
from pathlib import Path

import yaml

HERE = Path(__file__).resolve().parent
TAG = re.compile(r"v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?")
DEFAULT_TIMEOUT = 300  # seconds, for a step's run and its check each, and for each cleanup command
KUBECTL_TIMEOUT = 120  # seconds, for each kubectl call of the runner's own
SYSTEM_NAMESPACES = ("kube-system", "kube-public", "kube-node-lease", "local-path-storage")


def main():
    # A stopped task or a canceled CI job sends SIGTERM: clean up as on Ctrl-C.
    signal.signal(signal.SIGTERM, signal.default_int_handler)
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--kuberay-version", required=True, help="the tag under test, e.g. v1.8.0-rc.0")
    parser.add_argument("ids", nargs="*", metavar="ID", help="units to run; default: every unit without a result")
    args = parser.parse_args()
    version = args.kuberay_version
    if not TAG.fullmatch(version):
        sys.exit(f"--kuberay-version {version}: want a tag like v1.8.0-rc.0")

    # The cleanup deletes CRDs and namespaces: never on a cluster that matters.
    context = output("kubectl", "config", "current-context")
    if not context.startswith("kind-"):
        sys.exit(f"current context {context!r} is not a kind cluster")
    if output("kubectl", "get", "--raw", "/readyz", "--request-timeout=10s") != "ok":
        sys.exit(f"cluster {context} is not reachable")

    units = yaml.safe_load((HERE / "manifest.yaml").read_text())["units"]
    by_id = {unit["id"]: unit for unit in units}
    if unknown := [unit_id for unit_id in args.ids if unit_id not in by_id]:
        sys.exit(f"no unit in manifest.yaml: {' '.join(unknown)}")
    results = HERE / "results" / version
    if args.ids:
        todo = [by_id[unit_id] for unit_id in args.ids]
    else:
        todo = [unit for unit in units if not (results / unit["id"] / "result.json").exists()]

    # Steps see the tag under test, never the agent's credentials.
    env = {k: v for k, v in os.environ.items() if not k.startswith(("ANTHROPIC_", "CLAUDE_CODE_"))}
    env["KUBERAY_VERSION"] = version
    env["CHART_VERSION"] = version.removeprefix("v")

    failures = 0
    for unit in todo:
        print(f"{unit['id']} ... ", end="", flush=True)
        result = run_unit(unit, results / unit["id"], version, env)
        if result["status"] == "pass":
            print(f"pass ({result['seconds']}s)", flush=True)
        else:
            failures += 1
            print(f"fail at step {result['failed_step']}: {result['reason']} ({result['seconds']}s)", flush=True)
    print(f"{len(todo) - failures} pass, {failures} fail; results in {results}")
    sys.exit(1 if failures else 0)


def run_unit(unit, out_dir, version, env):
    shutil.rmtree(out_dir, ignore_errors=True)
    (out_dir / "steps").mkdir(parents=True)
    start = time.monotonic()
    rows = [{"name": step.get("name", step["run"]), "status": "not run"} for step in unit["steps"]]
    failed_step, reason = None, ""
    with tempfile.TemporaryDirectory(prefix="release-test-") as work:
        shell = Shell(work, env)
        try:
            for num, (step, row) in enumerate(zip(unit["steps"], rows), 1):
                log = out_dir / "steps" / f"{num:02}.log"
                reason, took = shell.step(step, log)
                row.update(
                    status=f"fail: {reason}" if reason else "pass",
                    seconds=took,
                    log=f"steps/{log.name}",
                    lines=log.read_text(errors="replace").count("\n"),
                )
                if reason:
                    failed_step = num
                    save_evidence(out_dir / "evidence")
                    break
        finally:
            # Also on Ctrl-C; a second Ctrl-C stops it.
            shell.cleanup(unit.get("cleanup", []), out_dir / "steps" / "cleanup.log")
    result = {
        "item": unit["id"],
        "version": version,
        "status": "fail" if failed_step else "pass",
        "failed_step": failed_step,
        "reason": reason,
        "seconds": int(time.monotonic() - start),
        "steps": rows,
    }
    (out_dir / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    return result


class Shell:
    """Runs a unit's scripts with bash -e in a working directory they share."""

    def __init__(self, work, env):
        self.work = work
        self.out = Path(work, ".out")  # the current step's run output; steps see it as $OUT
        self.env = {**env, "OUT": str(self.out)}
        self.groups = []  # process groups started, killed when the unit ends

    def step(self, step, log_path):
        """Runs a step and logs it; returns why it failed ("" when it passed) and the seconds it took."""
        timeout = step.get("timeout", DEFAULT_TIMEOUT)
        with open(log_path, "a") as log:
            if "name" in step:
                log.write(f"# {step['name']}\n")
            log.write(prefix_lines(step["run"], "$ ", "  "))
            if step.get("check"):
                log.write(prefix_lines(step["check"], "# check: ", "#        "))
            log.write("\n")
            start = time.monotonic()
            # A new file, not a truncated one: what an earlier step left running still writes to the old one.
            self.out.unlink(missing_ok=True)
            with open(self.out, "w") as out:
                code, timed_out = self.bash(step["run"], out, timeout)
            log.write(self.out.read_text(errors="replace"))
            phase = "run"
            if code == 0 and not timed_out and step.get("check"):
                log.write("\n# check\n")
                code, timed_out = self.bash(step["check"], log, timeout)
                phase = "check"
            took = int(time.monotonic() - start)
            log.write(f"\n# {phase} exit {code} after {took}s\n")
        if timed_out:
            return f"{phase} timed out after {timeout}s", took
        if code != 0:
            return f"{phase} exit {code}", took
        return "", took

    def cleanup(self, commands, log_path):
        """Runs every cleanup command whatever the others return, then kills what the unit left running."""
        try:
            with open(log_path, "a") as log:
                for command in commands:
                    log.write(prefix_lines(command, "$ ", "  "))
                    start = time.monotonic()
                    code, timed_out = self.bash(command, log, DEFAULT_TIMEOUT)
                    if timed_out:
                        log.write(f"# timed out after {DEFAULT_TIMEOUT}s\n\n")
                    else:
                        log.write(f"# exit {code} after {int(time.monotonic() - start)}s\n\n")
        finally:
            for pgid in self.groups:
                kill_group(pgid)
            self.groups = []

    def bash(self, script, out, timeout):
        """Runs script in its own process group, so a timeout kills everything it started and what it
        leaves running (port-forwards) dies with the unit. out must be a file: with a pipe, the wait
        would block on those leftovers."""
        out.flush()
        proc = subprocess.Popen(
            ["bash", "-ec", script],
            cwd=self.work,
            env=self.env,
            stdin=subprocess.DEVNULL,
            stdout=out,
            stderr=subprocess.STDOUT,
            start_new_session=True,
        )
        self.groups.append(proc.pid)
        try:
            return proc.wait(timeout), False
        except subprocess.TimeoutExpired:
            kill_group(proc.pid)
            return proc.wait(), True
        except KeyboardInterrupt:
            # The group is not in the terminal's foreground, so it did not see the Ctrl-C.
            kill_group(proc.pid)
            raise


def save_evidence(directory):
    """Saves what triage needs before the cleanup deletes it."""
    directory.mkdir()

    def save(name, *args):
        with open(directory / name, "a") as f:
            try:
                subprocess.run(["kubectl", *args], stdout=f, stderr=subprocess.STDOUT, timeout=KUBECTL_TIMEOUT)
            except subprocess.TimeoutExpired:
                f.write(f"kubectl {' '.join(args)}: timed out\n")

    save("pods.txt", "get", "pods", "-A", "-o", "wide")
    save("events.txt", "get", "events", "-A", "--sort-by=.lastTimestamp")
    # Only the Ray CRDs installed: one missing kind fails the whole get.
    crds = [name.split("/", 1)[1] for name in output("kubectl", "get", "crd", "-o", "name").split()]
    if ray := [crd for crd in crds if crd.endswith(".ray.io")]:
        save("ray-resources.yaml", "get", ",".join(ray), "-A", "-o", "yaml")
    pods = json.loads(output("kubectl", "get", "pods", "-A", "-o", "json") or "{}").get("items", [])
    pods = [pod["metadata"] for pod in pods if pod["metadata"]["namespace"] not in SYSTEM_NAMESPACES]
    for namespace in sorted({pod["namespace"] for pod in pods}):
        save("describe-pods.txt", "describe", "pods", "-n", namespace)
    for pod in pods:
        save("pod-logs.txt", "logs", "-n", pod["namespace"], pod["name"], "--all-containers", "--prefix", "--tail=500")


def kill_group(pgid):
    try:
        os.killpg(pgid, signal.SIGKILL)
    except ProcessLookupError:
        pass


def output(*cmd):
    """Returns cmd's stdout, or "" when it does not finish in time."""
    try:
        return subprocess.run(cmd, capture_output=True, text=True, timeout=KUBECTL_TIMEOUT).stdout.strip()
    except subprocess.TimeoutExpired:
        return ""


def prefix_lines(text, first, rest):
    """Prefixes the first line of text with first and the others with rest."""
    return first + text.rstrip("\n").replace("\n", "\n" + rest) + "\n"


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        # Ctrl-C or SIGTERM, after run_unit cleaned up.
        print("\ninterrupted; units without a result run again next time", file=sys.stderr)
        sys.exit(130)
