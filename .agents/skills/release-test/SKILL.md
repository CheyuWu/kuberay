---
name: release-test
description: >-
  Validate a KubeRay release candidate on a local kind cluster: plan changed Items, run the manifest,
  triage failures, write the summary. Use for `/release-test v1.8.0-rc.0` or when asked to run KubeRay
  release validation.
---

# Release test

The argument is the tag under test, for example `v1.8.0-rc.0`. Ask for it if it is missing.

1. **Check the release exists.** `helm repo add kuberay https://ray-project.github.io/kuberay-helm/`,
   `helm repo update`, then `helm search repo kuberay/kuberay-operator --devel --version <tag without v>`
   must list it. If not, stop: there is nothing to test yet.
2. **Plan what changed.** Run the `release-test-plan` skill with the tag. It finds the Items whose unit
   is missing or whose source changed since it was planned, and plans them. If it changed
   `release-test/manifest.yaml`, show `git diff --stat release-test/manifest.yaml` and stop until the
   user has reviewed the diff. Never run units nobody reviewed.
3. **Cluster.** The current kube context must be a kind cluster; create one with
   `kind create cluster --name release-test` if there is none.
4. **Run.** Start `uv run release-test/run.py --kuberay-version <tag>` in the background; it takes
   hours. It resumes where it stopped and writes `release-test/results/<tag>/<id>/result.json` per
   unit. Do not poll it; wait for it to exit.
5. **Triage.** For each `result.json` with `"status": "fail"` and no `triage.md` next to it, run the
   `release-test-run` skill on its directory in a subagent of its own, so the logs stay out of this
   context. Several can run at once.
6. **Summary.** Write `release-test/results/<tag>/SUMMARY.md` from `items.yaml`, the `result.json`
   files and the first line of each `triage.md`, in this shape, leaving out empty sections:

   ```markdown
   # Release test summary

   KubeRay v1.8.0-rc.0, 2026-11-02. 5 pass, 1 fail, 4 manual, 1 not run.

   ## Fail

   | Item | Step | Triage | Brief |
   |---|---|---|---|
   | [raycluster-quick-start](raycluster-quick-start/triage.md) | 2: check timed out after 900s | doc-bug | doc says 2 workers, sample has 1 |

   ## Manual

   Needs hardware or a cloud a kind cluster cannot provide; a human runs these.

   | Item | Requires | Source |
   |---|---|---|
   | gpu | gpu | https://docs.ray.io/en/master/kuberay/user-guides/gpu.html |

   ## Not run

   | Item | Why |
   |---|---|
   | rayservice-quick-start | no unit in manifest.yaml |

   ## Pass

   - ray-cluster-sample
   ```

   An Item without `requires` is under Not run when it has no unit or no `result.json`; say which.
7. **Report.** Give the user the Fail table. For each `plan-bug`, offer to apply the fix from its
   `triage.md` to the manifest and re-run the Item with
   `uv run release-test/run.py --kuberay-version <tag> <id>`, then update SUMMARY.md. Never open
   GitHub issues yourself.
