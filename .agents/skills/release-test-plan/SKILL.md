---
name: release-test-plan
description: >-
  Find the release test Items whose unit is missing or whose source changed, and write their units in
  release-test/manifest.yaml, copying the doc's commands into checked steps. Plan only, no cluster. Use
  before a release test run or when asked to plan release test Items.
---

# Release test plan

Turn Items from `release-test/items.yaml` into units in `release-test/manifest.yaml`.
`release-test/run.py` judges each step only by exit codes, so the quality of every `check` is the
quality of the release test.

Never run kubectl, helm, kind or docker here.

The arguments are the tag under test, for example `v1.8.0-rc.0`, and optionally Item ids. Ask for the
tag if it is missing.

## Find what to plan

Look at the Items you were given, or at every Item. Skip Items with `requires`: a human runs them.

1. Get the sources. Docs pages live in the ray repository, samples in this one:

   ```sh
   test -d release-test/.cache/ray || git clone --filter=blob:none --no-checkout https://github.com/ray-project/ray release-test/.cache/ray
   git -C release-test/.cache/ray fetch origin master
   git fetch --tags https://github.com/kuberay-project/kuberay
   ```

2. Each Item's file:
   - a docs.ray.io page: `doc/source/` plus the URL's path after `/en/master/`, `.html` replaced by
     `.md` (`.rst` or `.ipynb` when there is no `.md`), in `release-test/.cache/ray`;
   - a sample: its `source` path in this repository, at the tag;
   - a `procedure`: the Item itself.
3. Compare it with what its unit was planned from, the unit's `source_ref`: a ray commit for a page,
   a KubeRay tag for a sample.

   ```sh
   git -C release-test/.cache/ray diff <source_ref> origin/master -- doc/source/<path>.md   # a page
   git diff <source_ref> <tag> -- <path>                                                    # a sample
   ```

   - No unit yet: plan it.
   - Empty diff: nothing to do.
   - The diff changes only prose, comment lines, KubeRay versions or output no check reads: set
     `source_ref` to the new ref and leave the steps alone.
   - Otherwise re-plan only the steps the diff touches, and set `source_ref`. Keep the rest of the
     unit: it carries fixes from earlier runs.
   - The file is gone:
     `git -C release-test/.cache/ray diff -M --name-status <source_ref> origin/master -- doc/source/kuberay/`
     shows where it went. A moved page: fix the Item's `source` in items.yaml and plan from the new
     file. A removed page: report it to the user and plan nothing.
   - A `procedure` Item: plan it when it has no unit or when you were given its id.

   Also report units whose id is not in items.yaml.
4. Read what you plan from: `git -C release-test/.cache/ray show origin/master:doc/source/<path>.md`
   for a page, `git show <tag>:<path>` for a sample. Do not fetch docs.ray.io HTML. Read other files
   the page includes the same way, and only when a step depends on their content.
5. Write the unit and put it in manifest.yaml in items.yaml order, replacing the unit with the same id.
6. Check what you wrote: `yq '.units[].id' release-test/manifest.yaml` parses and lists each id once,
   and no `run`, `check` or `cleanup` names `master`, `latest` or a fixed KubeRay version.
7. Print a table for the reviewer: id, what changed in the source, number of steps, notes. Stop there;
   a human reviews `git diff release-test/manifest.yaml` before anything runs.

## Unit format

```yaml
  - id: raycluster-quick-start   # the Item id
    source_ref: 7e8a1e989bd07ed9a69d982883eb9e49e5598247   # page: `git -C release-test/.cache/ray rev-parse origin/master`; sample: the tag; procedure: none
    notes:                       # optional; what the reviewer and the report reader should know
      - "doc-bug: step 3 output shows 2 workers, the sample has 1"
    steps:
      - name: Install the KubeRay operator
        run: |                   # the page's commands, as the page writes them
          helm repo add kuberay https://ray-project.github.io/kuberay-helm/
          helm repo update
          helm install kuberay-operator kuberay/kuberay-operator --version $CHART_VERSION
        check: kubectl wait deploy/kuberay-operator --for=condition=Available --timeout=300s
        timeout: 300             # seconds, for run and for check each; default 300
    cleanup:
      - helm uninstall kuberay-operator
```

## Writing steps

- `run` holds the page's commands verbatim and in order, one code block or one numbered step of the
  page per step. Change only:
  - KubeRay versions: raw URLs and refs use `$KUBERAY_VERSION`, chart `--version` uses
    `$CHART_VERSION`, images use `quay.io/kuberay/operator:$KUBERAY_VERSION`. Never master, latest or
    a fixed KubeRay version.
  - Third-party installs (cert-manager, Prometheus, Kueue, Volcano): pin the exact version the page
    names, or the current release if it names none.
  - What cannot run unattended: drop `-it` and `--tty`, replace an interactive shell with the commands
    the page types into it, and run what the page runs on a laptop with Ray installed
    (`ray job submit`, Python scripts) in the head Pod:
    `kubectl exec "$(kubectl get pod -l ray.io/node-type=head -o name)" -- ...`.

  Name every such change in `notes`. A step the page does not have (removing one install method
  before trying the next) says so in its `name`.
- Everything the page states without a command goes in `check`: the waits it describes in prose
  ("wait until the Pods are Running"), the counts and the output it shows. Every step that the page
  makes a claim about gets a `check`. Write the claim, not the output:
  - Waiting: `kubectl wait --for=condition=...` or `--for=jsonpath='{.status.state}'=ready` with a
    `--timeout`. Never `sleep` and then look.
  - Counting: `test "$(kubectl get pods -l ray.io/node-type=worker -o name | wc -l)" -eq 2`.
  - Output: `grep -q 'pattern' "$OUT"`, where `$OUT` holds the step's `run` output.
  - Reading YAML or JSON: `yq` (mikefarah v4; `yq -p json -o yaml` for JSON).
  - Never match Pod names, IPs, timestamps or whole outputs.
- When the page claims a behaviour (failover, autoscaling, restart, upgrade), add the step that
  provokes it, then check the result.
- A step that is expected to fail checks that: `run: kubectl ray get token no-auth || true` with
  `check: grep -q 'not configured' "$OUT"`.
- Background processes redirect their output and are left running; the runner kills them when the
  unit ends: `kubectl port-forward svc/raycluster-kuberay-head-svc 8265:8265 >/dev/null 2>&1 &`. The
  next step's check retries: `curl -sf --retry 10 --retry-delay 2 --retry-all-errors ...`.
- Steps run with `bash -e`, stdin closed, in a temp directory the unit's steps share. Nothing is
  interactive.
- `cleanup` deletes everything the unit created, also when the unit fails halfway: every command runs
  whatever the others return, so use `--ignore-not-found`. Nothing else cleans up after a unit.
- Sections the page runs on a cloud (GKE, EKS) are left out and named in `notes`. So is
  `kind create cluster` and `kind delete cluster`: the cluster belongs to whoever runs the manifest.
- Samples (`source` is a repo path): install the operator, `kubectl apply -f
  https://raw.githubusercontent.com/kuberay-project/kuberay/$KUBERAY_VERSION/<path>`, wait until ready,
  then check the one behaviour the sample exists to show (an auth sample rejects an unauthenticated
  request; an autoscaler sample scales up under load).
- A kind node has about 8 CPU and 16 GiB. If the page's resources do not fit, lower the requests and
  say so in `notes`.
- Problems you find while planning (the page pins master, a link is dead, the page and the sample
  disagree) go in `notes` as `doc-bug:` or `sample-bug:` one-liners. Quote every note.
