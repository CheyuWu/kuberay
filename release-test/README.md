# Release test

Validates a KubeRay release candidate on a local kind cluster against the docs on docs.ray.io, the samples in
`ray-operator/config/samples/`, the Helm charts and the Go modules. It replaces the release validation
spreadsheet.

`run.py` runs the steps in `manifest.yaml`, judges them by exit code alone, writes the logs and cleans up.
Everything else is done by the agent skills in `.agents/skills/`: `release-test-plan` finds the Items whose
source changed with `git diff` and turns their doc commands into steps, `release-test-run` explains a
failure, and `release-test` drives a whole run and writes the summary.

| File | Written by | Committed |
|---|---|---|
| `items.yaml` | humans: what to validate | yes |
| `manifest.yaml` | the `release-test-plan` skill, reviewed like code: how to validate each Item | yes |
| `results/<tag>/` | `run.py`, then the `release-test` and `release-test-run` skills | no |

## Running a release

You need kind, kubectl, helm, git, [yq v4](https://github.com/mikefarah/yq), [uv](https://github.com/astral-sh/uv)
and a coding agent that loads `.agents/skills/`. Run the commands from the repository root.

`/release-test v1.8.0-rc.0` does all of it:

1. `/release-test-plan v1.8.0-rc.0` plans the Items that are new or whose page or sample changed since their
   unit was planned. Review `git diff release-test/manifest.yaml` and commit it.
2. Run the manifest. It resumes where it stopped; pass IDs to re-run single Items.

   ```sh
   kind create cluster --name release-test
   uv run release-test/run.py --kuberay-version v1.8.0-rc.0
   ```

3. `/release-test-run release-test/results/v1.8.0-rc.0/<id>/` triages each failure into its `triage.md`.
4. The `release-test` skill writes `results/v1.8.0-rc.0/SUMMARY.md` from the results and the triage.

## items.yaml

```yaml
- id: raycluster-quick-start           # stable slug; the unit id and results/<tag>/<id>/
  source: https://docs.ray.io/en/master/kuberay/getting-started/raycluster-quick-start.html
- id: ray-cluster-sample
  source: ray-operator/config/samples/ray-cluster.sample.yaml
- id: ray-cluster-tpu-v6e-singlehost
  source: ray-operator/config/samples/ray-cluster.tpu-v6e-singlehost.yaml
  requires: [tpu, gke]                 # not runnable on kind: listed under Manual, never planned or run
- id: go-get-ray-operator
  procedure: |                         # for Items that are neither a page nor a sample
    In an empty Go module, go get github.com/ray-project/kuberay/ray-operator@$KUBERAY_VERSION succeeds ...
```

## manifest.yaml

Each step's `run` is the page's commands as written, and its optional `check` asserts what the page claims
without a command; a step passes when both exit 0. Commands use `$KUBERAY_VERSION` and `$CHART_VERSION`, never
a fixed version, so the manifest carries over from one release to the next. `source_ref` is what the unit was
planned from: the ray commit for a page, the KubeRay tag for a sample, so `git diff <source_ref>` shows what
changed since. The format is in `.agents/skills/release-test-plan/SKILL.md`.

## Results

```text
results/<tag>/
├── SUMMARY.md           the release-test skill: Fail, Manual, Not run and Pass
└── <item-id>/
    ├── result.json      run.py: status, failed_step, reason and a row per step
    ├── steps/NN.log     the command, the check, the output, the exit code
    ├── evidence/        on failure: pods, events, Ray resources, describe, Pod logs
    └── triage.md        on failure, the release-test-run skill: verdict, cause, fix
```

The verdict is one of `plan-bug` (the manifest is wrong), `doc-bug`, `sample-bug`, `product-bug`, `env` (kind
cannot do it) or `flaky`. Nothing files GitHub issues; a human decides.
