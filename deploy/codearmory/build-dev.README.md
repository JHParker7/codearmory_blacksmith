# build-dev — alpha image on merge to `dev`

Wires the missing "build after dev" step for the **local dev cluster**: when a PR
merges to the `dev` branch, build the project's image and push it to the
in-cluster registry tagged with the next **semantic version + `a`** (alpha),
e.g. `v0.1.1a`.

## Pieces

- `build-dev.workflow.json` — the pipeline (workflows `POST /pipelines`):
  `clone dev → compute semver → forge/build-image → show`.
- `build-dev.trigger.json` — the events trigger (`POST /triggers`): matches
  `repo.pull_request.merged` with `data.target_ref == "dev"` and runs the
  pipeline, templating `namespace`/`repo`/`ref` from the event payload. Its
  `pipeline_id` is filled in by the register script.
- `register-build-dev.sh` — registers both against the local cluster. Needs a
  **local** gatekeeper token (`CODEARMORY_TOKEN`), not the dead exp one.

## Versioning policy

Read the highest git tag matching `v<M>.<m>.<p>` (any trailing letter, i.e. the
alpha marker, is stripped first), bump **patch**, and append `a` for the dev
image:

```
v0.1.0 (tag)  ->  next v0.1.1  ->  image tag v0.1.1a
```

The alpha tag is pushed back to the repo, so the next merge-to-dev bumps again
(`v0.1.1a → v0.1.2a`), giving monotonic alphas without a manual release step.
Seed the line with `git tag v0.1.0` on the repo to start at `v0.1.1a` as in the
example; with no tags at all it starts at `v0.0.1a`. To bump minor/major
instead, change the one `P=$((P+1))` line in the `version` step.

## Registry

Pushes to `registry-local.codearmory.svc.cluster.local:5000/<repo>:<version>a`
— the in-cluster registry, already in forge's `FORGE_INSECURE_REGISTRIES`. No
registry auth secret is needed (unlike the old exp/Forgejo destination).

## Notes / not-yet-verified

- Not yet registered or run live — pending a local gatekeeper token (the auto
  classifier blocks reading/minting one from this session).
- The build uses the repo-root `Dockerfile` (the one the devops stage authors).
- If `target_ref` ever arrives as `refs/heads/dev` rather than `dev`, adjust the
  trigger match value accordingly.
