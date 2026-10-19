#!/usr/bin/env python3
"""CPU used per compose service and per host process over a window (make loadtest-profile).

    cpu_usage.py --seconds 25 [--delay 5] [--project mcp-gateway]

Reads each container's cgroup CPU counter (cgroup v1 cpuacct or v2 cpu.stat), the utime+stime of
the host processes named in HOST_PROCS from /proc, and the machine's busy time from /proc/stat, at
the start and the end of the window, and prints the average cores each one used. Linux only.
"""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
import time
from pathlib import Path

HOST_PROCS = ("loadgen", "docker-proxy", "dockerd", "containerd")
CLK_TCK = os.sysconf("SC_CLK_TCK")


def containers(project: str) -> dict[str, str]:
    """Return {service: full container id} for the running containers of a compose project."""
    out = subprocess.run(
        [
            "docker",
            "ps",
            "--no-trunc",
            "--filter",
            f"label=com.docker.compose.project={project}",
            "--format",
            '{{.ID}} {{.Label "com.docker.compose.service"}}',
        ],
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    return {svc: cid for cid, svc in (line.split() for line in out.splitlines() if line.strip())}


def cgroup_seconds(cid: str) -> float:
    """CPU seconds used by a container so far (cgroup v1 or v2, cgroupfs or systemd driver)."""
    v1 = Path(f"/sys/fs/cgroup/cpuacct/docker/{cid}/cpuacct.usage")
    if v1.exists():
        return int(v1.read_text()) / 1e9
    for p in (
        Path(f"/sys/fs/cgroup/system.slice/docker-{cid}.scope/cpu.stat"),
        Path(f"/sys/fs/cgroup/docker/{cid}/cpu.stat"),
    ):
        if p.exists():
            for line in p.read_text().splitlines():
                key, value = line.split()
                if key == "usage_usec":
                    return int(value) / 1e6
    raise FileNotFoundError(f"no cgroup CPU counter for container {cid[:12]}")


def process_seconds(names: tuple[str, ...]) -> dict[str, float]:
    """CPU seconds (utime + stime) of the live processes per comm name, in one pass over /proc."""
    ticks = dict.fromkeys(names, 0)
    for stat in Path("/proc").glob("[0-9]*/stat"):
        try:
            text = stat.read_text()
        except OSError:
            continue
        comm = text[text.index("(") + 1 : text.rindex(")")]
        if comm in ticks:
            fields = text[text.rindex(")") + 2 :].split()
            ticks[comm] += int(fields[11]) + int(fields[12])
    return {name: t / CLK_TCK for name, t in ticks.items()}


def busy_seconds() -> float:
    """Machine-wide non-idle CPU seconds from /proc/stat."""
    fields = [int(x) for x in Path("/proc/stat").read_text().splitlines()[0].split()[1:]]
    idle = fields[3] + fields[4]
    return (sum(fields[:8]) - idle) / CLK_TCK


def snapshot(ids: dict[str, str]) -> dict[str, float]:
    snap = {svc: cgroup_seconds(cid) for svc, cid in ids.items()}
    snap.update({f"host:{name}": sec for name, sec in process_seconds(HOST_PROCS).items()})
    snap["machine (busy)"] = busy_seconds()
    return snap


def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--seconds", type=float, default=25.0, help="length of the window")
    ap.add_argument("--delay", type=float, default=0.0, help="wait before the window starts (warm-up)")
    ap.add_argument("--project", default="mcp-gateway", help="compose project name")
    args = ap.parse_args(argv)
    time.sleep(args.delay)
    ids = containers(args.project)
    if not ids:
        print(f"no running containers in compose project {args.project}", file=sys.stderr)
        return 1
    start, t0 = snapshot(ids), time.monotonic()
    time.sleep(args.seconds)
    end, t1 = snapshot(ids), time.monotonic()
    window = t1 - t0
    print(f"cpu_usage.py: {window:.1f} s window, {os.cpu_count()} CPUs; average cores used")
    for key in sorted(start, key=lambda k: -(end[k] - start[k])):
        print(f"  {key:<24} {(end[key] - start[key]) / window:6.2f}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
