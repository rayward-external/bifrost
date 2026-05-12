# Bifrost Main-to-ACR Auto Build Design

## Status

Approved in discussion on 2026-05-12 for spec write-up. Implementation has not started.

## Goal

When a change lands on `rayward-external/bifrost:main`, automatically build a new Bifrost container image and push it to the existing Azure Container Registry without requiring Azure CLI or Azure login on a developer machine.

The build will run in GitHub Actions, not in Azure. Deployment will continue to happen through the existing ACR webhook flow already implemented in the private infra repository.

## Scope

This design covers:

- A public-repo GitHub Actions workflow in `rayward-external/bifrost`
- A private-repo GitHub Actions workflow in `rayward-internal/llm-gateway-infra`
- One-time GitHub and Azure setup required for those workflows
- Verification steps and operational expectations

This design does not cover:

- Replacing the existing ACR webhook deployment path
- Adding Azure details to the public repository
- Building images on Azure ACR Tasks
- General CI cleanup unrelated to the Bifrost image publish path

## Decision Summary

### Chosen approach

Use a two-repository event-driven workflow:

1. `rayward-external/bifrost` listens for `push` to `main`
2. It sends a `repository_dispatch` event to `rayward-internal/llm-gateway-infra`
3. The private repo checks out the exact Bifrost commit SHA from the dispatch payload
4. The private repo builds the image in GitHub Actions from `transports/Dockerfile`
5. The private repo authenticates to Azure using GitHub Actions OIDC
6. The private repo pushes the image to ACR
7. The existing ACR webhook deploys the pushed digest to the live Container App

### Why this approach

- It keeps all Azure and ACR details in the private repo
- It avoids polling and reacts immediately to merges into `main`
- It uses the existing ACR webhook deployment path instead of introducing a second deploy mechanism
- It does not rely on any local machine state
- Current GitHub Actions minute usage for `rayward-internal` is low enough that GitHub-hosted builds are acceptable for now

## Alternatives Considered

### Option 1: Build in the public repo and push directly to ACR

Rejected.

- It would require Azure and registry details to exist in the public repo's workflow configuration or secrets surface
- It weakens the boundary between the public source repo and the private deployment repo
- It makes private deployment concerns too visible in the public fork

### Option 2: Scheduled polling from the private repo

Rejected earlier in discussion.

- It is harder to reason about
- It adds unnecessary delay
- It creates confusing behavior when a merge has landed but the build has not yet started

### Option 3: Build in Azure using ACR Tasks

Rejected for this phase.

- It would work technically, but the user explicitly chose to keep image builds in GitHub
- It adds Azure-side build orchestration that is not necessary at current scale
- It can be revisited later if GitHub-hosted build minutes become a concern

## Current State

### Public repo

- Repository: `rayward-external/bifrost`
- Existing upstream GitHub workflows are present under `.github/workflows/`
- No Rayward-specific image publish workflow exists yet
- Candidate image build file is `transports/Dockerfile`

### Private repo

- Repository: `rayward-internal/llm-gateway-infra`
- No `.github` workflow directory exists yet
- The existing Azure deployment path is an ACR webhook backed by a Logic App in `azure/modules/deploy_webhook/main.tf`
- Azure OIDC bootstrap guidance already exists in `scripts/bootstrap-cicd-aad.sh`

## Architecture

### High-level flow

```text
merge to rayward-external/bifrost main
  -> public GitHub Actions workflow
  -> repository_dispatch to rayward-internal/llm-gateway-infra
  -> private GitHub Actions workflow
  -> checkout exact bifrost SHA
  -> docker build using transports/Dockerfile
  -> azure/login via OIDC
  -> docker push to ACR as bifrost-rayward:<short-sha>
  -> ACR webhook fires
  -> Logic App updates Container App image by digest
```

### Trust boundaries

- Public repo knows only how to emit an event to the private repo
- Private repo owns Azure authentication, ACR hostname, tagging, and build execution
- Azure trusts the private repo workflow identity through federated credentials
- Deployment remains Azure-native after the image push

## Public Repository Design

### Workflow trigger

The public workflow triggers on:

- `push` to `main`

This matches the user intent: build only after changes have landed on the main branch, not on every PR update.

### Workflow responsibilities

The public workflow should:

- Read the current commit SHA and branch information from the GitHub Actions context
- Send a `repository_dispatch` event to `rayward-internal/llm-gateway-infra`
- Pass a small client payload containing:
  - source repository
  - ref
  - full SHA
  - short SHA
  - actor
  - source workflow run URL or run ID for traceability

The public workflow should not:

- Build Docker images
- Authenticate to Azure
- Contain ACR login server details
- Contain subscription IDs, tenant IDs, or resource-group details

### Authentication

The public repo cannot rely on the default `GITHUB_TOKEN` for cross-repo dispatch into a private repository owned elsewhere. It needs a dedicated credential stored as a GitHub Actions secret in `rayward-external/bifrost`.

Recommended secret:

- `RAYWARD_INFRA_DISPATCH_TOKEN`

Recommended credential properties:

- Fine-grained PAT or GitHub App token
- Minimal access needed to create `repository_dispatch` events for `rayward-internal/llm-gateway-infra`

### Failure behavior

If dispatch fails:

- The public workflow fails visibly
- No image build occurs
- No Azure deployment occurs

This is preferred over silently dropping the event.

## Private Repository Design

### Workflow triggers

The private workflow triggers on:

- `repository_dispatch` with type `bifrost-main-merged`
- `workflow_dispatch` for manual rebuilds and debugging

Manual dispatch allows recovery without forcing an extra commit into the public repo.

### Workflow permissions

The private workflow needs:

- `contents: read`
- `id-token: write`

`id-token: write` is required for `azure/login` OIDC federation.

### Checkout strategy

The workflow checks out:

1. The private infra repo as the workflow host repository
2. `rayward-external/bifrost` into a subdirectory at the exact SHA provided in the dispatch payload

The build must use the exact merged commit SHA instead of rebuilding `main` loosely. This avoids race conditions where a later merge changes `main` before the private workflow begins.

### Build context

Build source:

- Repository: `rayward-external/bifrost`
- Dockerfile: `transports/Dockerfile`
- Build context: repository root unless Dockerfile requirements force a narrower explicit context

The implementation must verify the correct context because the Dockerfile copies both `ui/` and `transports/` content.

### Tagging strategy

Required tag:

- `bifrost-rayward:<short-sha>`

Optional secondary tag:

- `bifrost-rayward:latest`

The deployment path should not depend on `latest`. The primary deployment identity should remain the immutable pushed digest derived from the short-SHA tag push.

### Azure authentication

The private workflow logs in with `azure/login` using repository secrets:

- `AZURE_CLIENT_ID`
- `AZURE_TENANT_ID`
- `AZURE_SUBSCRIPTION_ID`

These values correspond to the federated identity created by `scripts/bootstrap-cicd-aad.sh`.

No local Azure login is required.

### ACR push

After `azure/login`, the workflow logs Docker into the target ACR and pushes the built image.

Registry details remain private-repo configuration. They should be stored in the private repo as GitHub Actions variables or secrets, for example:

- `AZURE_ACR_LOGIN_SERVER`
- `AZURE_ACR_REPOSITORY`

The repository name should match the existing webhook scope, which currently expects pushes to `bifrost-rayward:*`.

### Deployment handoff

The private workflow does not run `az containerapp update`.

Instead:

- pushing the image to ACR triggers the existing webhook
- the Logic App receives the push payload
- the Logic App patches the Container App template to the new image reference, preferably by digest

This preserves one deploy mechanism and avoids drift between GitHub Actions deploy logic and Azure deploy logic.

## Data Contract Between Repositories

Dispatch event type:

- `bifrost-main-merged`

Dispatch payload fields:

- `source_repo`
- `source_ref`
- `source_sha`
- `source_short_sha`
- `source_actor`
- `source_run_id`

Recommended validation in the private workflow:

- ensure `source_repo == rayward-external/bifrost`
- ensure `source_sha` is non-empty
- fail fast if required payload fields are missing

## Secrets and Variables

### Public repo secrets

- `RAYWARD_INFRA_DISPATCH_TOKEN`

### Private repo secrets

- `AZURE_CLIENT_ID`
- `AZURE_TENANT_ID`
- `AZURE_SUBSCRIPTION_ID`

### Private repo variables or secrets

- `AZURE_ACR_LOGIN_SERVER`
- `AZURE_ACR_REPOSITORY`

If the ACR login server is considered sensitive in practice for the organization, store it as a secret rather than a variable.

## One-Time Setup

### Azure

Run or validate `scripts/bootstrap-cicd-aad.sh` from an Azure-admin-capable environment such as Azure Cloud Shell.

Expected outcome:

- an Azure application and service principal exist for the private repo workflow
- federated credentials trust `rayward-internal/llm-gateway-infra` branch execution
- the principal has `AcrPush` on the target ACR

If manual workflow dispatch on additional branches is needed later, the script can be extended to include more branch subjects.

### GitHub

Public repo:

- add `RAYWARD_INFRA_DISPATCH_TOKEN`

Private repo:

- add Azure OIDC secrets
- add ACR login server and repository configuration
- enable Actions if needed for the repository

## Error Handling

### Public workflow errors

- Dispatch credential missing or invalid: fail job
- Private repo unreachable or dispatch rejected: fail job

### Private workflow errors

- Missing or malformed dispatch payload: fail job before checkout
- Source SHA not found: fail job
- Docker build failure: fail job
- Azure OIDC login failure: fail job
- Docker push failure: fail job

### Downstream deployment errors

If the image push succeeds but deployment fails:

- the private workflow still reports a successful push
- Azure deployment troubleshooting occurs in the existing Logic App and Container App path

This boundary should be documented so operators know where to look.

## Observability

For traceability, the private workflow should log:

- source repo
- source SHA
- produced image reference
- ACR repository

The public workflow should expose enough metadata in the dispatch payload to correlate:

- public workflow run
- private workflow run
- pushed image tag

## Security Notes

- Do not commit Azure IDs, ACR login server values, or credentials into `rayward-external/bifrost`
- Do not store long-lived Azure client secrets for the workflow; use OIDC federation
- Keep the dispatch token scoped as narrowly as GitHub allows
- Build only the exact SHA received from the public repo event

## Verification Plan

### Repository-level verification

Public repo:

- validate workflow YAML syntax
- validate dispatch payload rendering

Private repo:

- validate workflow YAML syntax
- run a manual `workflow_dispatch` test against a known Bifrost SHA
- confirm image is pushed to the expected ACR repository
- confirm the existing webhook deploy path updates the Container App image

### End-to-end verification

1. Merge a trivial commit to `rayward-external/bifrost:main`
2. Confirm public workflow dispatches successfully
3. Confirm private workflow checks out the exact merged SHA
4. Confirm ACR contains `bifrost-rayward:<short-sha>`
5. Confirm the webhook deploy path updates the running Container App to that image digest

## Implementation Outline

1. Add public dispatch workflow in `rayward-external/bifrost`
2. Add private build-and-push workflow in `rayward-internal/llm-gateway-infra`
3. Update bootstrap/setup documentation where needed
4. Validate workflow syntax and event payload assumptions
5. Run manual and end-to-end verification

## Open Questions Resolved

### Where is the image built?

In GitHub Actions in the private infra repo.

### Where do Azure details live?

In private GitHub Actions secrets and variables plus Azure federated identity configuration, not in committed public-repo files and not on a developer machine.

### What deploys the image?

The existing ACR webhook and Logic App path, not a new direct deploy command in GitHub Actions.
