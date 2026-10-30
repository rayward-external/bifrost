#!/usr/bin/env bash
set -euo pipefail

# Two guards on harden-runner egress allowlists:
#
#   1. every job that runs apt-get under `egress-policy: block` allowlists the
#      Ubuntu APT hosts it will need;
#   2. every job that runs the provider harness allowlists each external host
#      the harness will dial - see the second python block for why that is not
#      the same set as "the hosts the collection sends requests to".
#
# harden-runner's `egress-policy: block` drops every destination absent from
# `allowed-endpoints`, so a job that shells out to `apt-get update` fails at the
# mirror fetch unless those hosts are listed. The failure only surfaces when the
# workflow actually runs - release-pipeline.yml triggers on push to main, not on
# pull requests - so a job added with a copied-but-incomplete allowlist reaches
# production untested and takes the release with it.
#
# Usage: ./check-egress-allowlist.sh [workflow.yml ...]
#   With no arguments, checks every workflow in .github/workflows.

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKFLOW_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
# Exported for the python blocks below. They run from stdin (`python3 - ...`),
# where __file__ is "<stdin>" rather than a path - deriving the repo root there
# silently resolves somewhere above the checkout, every input file reads as
# absent, and the check passes having examined nothing.
REPO_ROOT="$(cd "${WORKFLOW_DIR}/../.." && pwd)"
export REPO_ROOT

if [[ $# -gt 0 ]]; then
  FILES=("$@")
else
  FILES=()
  while IFS= read -r f; do FILES+=("$f"); done < <(find "${WORKFLOW_DIR}" -maxdepth 1 -name '*.yml' -o -maxdepth 1 -name '*.yaml' | sort)
fi

python3 - "${FILES[@]}" <<'PY'
import sys
import yaml

# Hosts apt-get needs on a GitHub-hosted ubuntu runner once apt-sources-https.sh
# has moved the sources to HTTPS. The images point at the Azure-local mirror over
# plain HTTP, which serves no HTTPS, so the script swaps it for archive.ubuntu.com;
# esm.ubuntu.com serves the Expanded Security Maintenance lists apt refreshes
# alongside it.
REQUIRED_APT_HOSTS = ("archive.ubuntu.com:443", "esm.ubuntu.com:443")
# arm64 Ubuntu pulls from the ports mirror (ubuntu-ports), never the archive one;
# the arm-64-2cpu-8gb image's apt sources point straight at ports.ubuntu.com.
REQUIRED_ARM64_APT_HOSTS = ("ports.ubuntu.com:443",)
# install-arm64-runner-deps.sh runs apt-get itself, so a step calling it never
# shows "apt-get" here; its `docker` component also adds Docker's apt repo.
ARM64_INSTALLER = "install-arm64-runner-deps.sh"
# Installers that run apt-get themselves, and switch the sources to HTTPS first.
APT_INSTALLERS = (ARM64_INSTALLER, "install-cross-compilers.sh")
APT_SOURCES_HTTPS = "apt-sources-https.sh"
# Docker's apt repo, plus apt's SRV probe for it, and the sigstore TUF root that
# the freshly installed dockerd fetches when buildx bootstraps its builder.
DOCKER_HOSTS = (
    "download.docker.com:443",
    "_https._tcp.download.docker.com:443",
    "tuf-repo-cdn.sigstore.dev:443",
)
# The `aws` component pip-installs awscli into a venv.
PYPI_HOSTS = ("pypi.org:443", "files.pythonhosted.org:443")

failures = []
checked = 0

for path in sys.argv[1:]:
    with open(path) as fh:
        doc = yaml.safe_load(fh) or {}

    for job_name, job in (doc.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        steps = job.get("steps") or []

        runs = [step.get("run") or "" for step in steps if isinstance(step, dict)]
        uses_apt = any("apt-get" in run or any(i in run for i in APT_INSTALLERS) for run in runs)
        if not uses_apt:
            continue
        installs_docker = any(
            ARM64_INSTALLER in line and "docker" in line.split(ARM64_INSTALLER, 1)[1].split()
            for run in runs
            for line in run.splitlines()
        )
        installs_aws = any(
            ARM64_INSTALLER in line and "aws" in line.split(ARM64_INSTALLER, 1)[1].split()
            for run in runs
            for line in run.splitlines()
        )

        harden = next(
            (
                step
                for step in steps
                if isinstance(step, dict)
                and "step-security/harden-runner" in (step.get("uses") or "")
            ),
            None,
        )
        if harden is None:
            continue

        with_block = harden.get("with") or {}
        if str(with_block.get("egress-policy", "")).strip() != "block":
            continue

        checked += 1
        allowed = set((with_block.get("allowed-endpoints") or "").split())
        # An inline apt-get update must switch the sources to HTTPS first, in the
        # same step: otherwise apt reaches for the plain-HTTP mirror, which no
        # allowlist carries any more, and fails as an opaque fetch timeout.
        for run in runs:
            update = run.find("apt-get update")
            if update != -1 and run.find(APT_SOURCES_HTTPS) not in range(0, update):
                failures.append((path, job_name, [f"{APT_SOURCES_HTTPS} before apt-get update"]))
        plain = sorted(e for e in allowed if e.endswith("ubuntu.com:80"))
        if plain:
            failures.append((path, job_name, [f"no plain-HTTP apt host ({', '.join(plain)})"]))
        is_arm = "arm" in str(job.get("runs-on") or "").lower()
        required = REQUIRED_ARM64_APT_HOSTS if is_arm else REQUIRED_APT_HOSTS
        if installs_docker:
            required += DOCKER_HOSTS
        if installs_aws:
            required += PYPI_HOSTS
        missing = [host for host in required if host not in allowed]
        if missing:
            failures.append((path, job_name, missing))

for path, job_name, missing in failures:
    print(f"FAIL {path} :: job '{job_name}' runs apt-get under egress-policy: block "
          f"but needs {', '.join(missing)}", file=sys.stderr)

if failures:
    print(f"\n{len(failures)} of {checked} apt-using blocked-egress job(s) have an "
          f"incomplete allowlist.", file=sys.stderr)
    sys.exit(1)

print(f"OK: all {checked} apt-using blocked-egress job(s) allowlist the Ubuntu APT hosts.")
PY

# ---------------------------------------------------------------------------
# 2. Provider-harness hosts
# ---------------------------------------------------------------------------
#
# The harness sends nearly every request to 127.0.0.1:8080, so its collection
# looks like it needs no egress at all. It does: bifrost-http runs on the SAME
# runner and makes the upstream provider calls, and it downloads any URL passed
# as a document/image source for providers whose APIs cannot take a URL
# directly. Those fetches leave the runner and are governed by the same
# allowlist.
#
# That combination is why this check exists. A blocked host surfaces as a
# connection error attributed to the provider, so it reads as a flaky provider
# rather than a firewall - and release-pipeline.yml only runs on push to main,
# so the first time anyone sees it is during a real release.
#
# The rule enforced here is deliberately blunt: every external host appearing in
# the harness collection or its seeded provider config must be either
# allowlisted or listed in NOT_DIALLED_HOSTS with a reason. Classifying
# automatically would mean deciding which URLs Bifrost fetches and which the
# provider fetches, which depends on per-provider API shape and would silently
# get it wrong. Forcing a human to say which one it is cannot.

python3 - "${FILES[@]}" <<'PY'
import json
import os
import re
import sys
import yaml

REPO_ROOT = os.environ["REPO_ROOT"]
COLLECTION = os.path.join(REPO_ROOT, "tests/e2e/api/collections/provider-harness.json")
PROVIDER_CONFIG = os.path.join(REPO_ROOT, "tests/integrations/python/config.json")

# A missing input means this check verified nothing, which must be an error and
# not a silent pass - that is the exact failure mode the REPO_ROOT export fixes.
for required_input in (COLLECTION, PROVIDER_CONFIG):
    if not os.path.exists(required_input):
        print(f"::error::egress check input not found: {required_input}", file=sys.stderr)
        sys.exit(1)

# Scripts that mean "this job runs the provider harness".
HARNESS_MARKERS = ("test-core.sh", "test-provider-harness.sh", "test-proxy-e2e.sh")

# Hosts that appear in the sources scanned above but that the runner never
# dials, each with the reason it is exempt. Anything not listed here and not
# allowlisted fails this check, so a new host forces an explicit decision rather
# than being quietly assumed harmless.
NOT_DIALLED_HOSTS = {
    # Fetched by the provider's own server-side tooling, never by Bifrost.
    "example.com": "MCP server_url and web_fetch targets - dereferenced by the provider",
    "mcp.deepwiki.com": "Anthropic mcp_servers connector URL in a request body - Anthropic's API dereferences it server-side, Bifrost only forwards the URL string",
    "www.youtube.com": "Gemini video input - Google fetches the URL server-side",
    "www.wikipedia.org": "Gemini urlContext tool input (\"what is https://www.wikipedia.org/ about?\") - Google fetches the URL server-side, same as www.youtube.com",
    "en.wikipedia.org": "googleSearch excludeDomains filter value, not a URL anyone fetches",
    "placeholder.search.windows.net": "Azure AI Search placeholder in a request body; never resolved",
    "replace-me.search.windows.net": "azureSearchEndpoint collection-variable placeholder, meant to be overridden locally with real credentials; same pattern as placeholder.search.windows.net",
    "does-not-exist-h146.search.windows.net": "148.6 [EXPECT-4XX] azure On Your Data data_sources endpoint - Azure OpenAI's On-Your-Data feature dereferences it server-side, Bifrost only forwards the URL string",
    "petstore3.swagger.io": "OpenAPI/Swagger servers/base_url URL embedded in openapi_config.spec request bodies across the openapi mcp-openapi-spec preview and tools_to_execute registration tests - these assert the spec is parsed into tools and that base_url/tools_to_execute are echoed back in the client config, never a tool_call round trip through a real chat completion that would make Bifrost dial the petstore API itself",
    # Deliberately unresolvable - negative tests assert the failure path.
    "bifrost.invalid": "reserved .invalid TLD; negative-path tests assert it fails to resolve",
    "harness-lib-installed.invalid": "reserved .invalid TLD; connection_url fixture value in the library 'installed' flag registration test - asserts registration/listing behavior, never dialled",
    "harness.example": "reserved RFC 2606 .example TLD; redirect_uri value registered via POST /oauth2/register and echoed back through GET /oauth2/authorize in the raw-path-auth regression (#5765) - matchRedirectURI (mcpoauth2issuance.go) only string-compares it against the registered value, never dereferences it",
    "unlisted.example": "reserved .example TLD; [EXPECT-400] openai oauth-redirect-policy remote callback rejected - redirect_uri is string-compared against the registered value, never dereferences it, same mechanism as harness.example",
    "idp.example": "reserved .example TLD; oauth_key_config token_url in three [EXPECT-400] save-rejection tests (half-oauth, http-oauth, query-auth-style) - the save itself is refused, so the config is never persisted or activated",
    "does-not-resolve.invalid": "reserved .invalid TLD; POST /api/mcp/client connection_string in the harness's public-address-requirement test, which asserts a 403 before any dial - same assertion as the Go unit tests in mcpregistrationguard_test.go (rejectPrivateMCPTargetIfAuthBypassed rejects it pre-dial)",
    "does-not-exist-h150.invalid": "152.2 [EXPECT-4XX] openai responses mcp tool server_url - OpenAI's Responses API dereferences the MCP server_url server-side, Bifrost only forwards the URL string, same pattern as mcp.deepwiki.com",
    "dbx-fixture-workspace.invalid": "databricks service-principal OAuth token_url (#8254) - routed through the harness's own local proxy fixture (tests/e2e/api/runners/databricks-token-fixture.mjs, 127.0.0.1:8795 by default) which terminates TLS for the workspace host itself; per the test's own prerequest-script comment, no request leaves the machine",
    "harness-endpoint-guard.example.invalid": "reserved .example.invalid; POST /api/providers/azure/keys endpoint value in the provider-endpoint-updates-require-dashboard-auth test, which asserts the write is refused (403) and the key never persisted - config data stored/compared, never dialed",
    "harness-credential-guard.example.invalid": "reserved .example.invalid; [EXPECT-401] vertex key-credential-dial-targets test - the management API call itself is refused (401, missing setup token) before the credential JSON is ever read, same guard mechanism as harness-endpoint-guard.example.invalid",
    "harness-plugin-guard.example.invalid": "reserved .example.invalid; [EXPECT-401] openai plugin-config-dial-targets test - the management API call itself is refused (401, missing setup token) before the otel config is ever read, same guard mechanism as harness-endpoint-guard.example.invalid",
    "evil.example": "reserved RFC 2606 .example TLD, multiple [EXPECT-400]/assertion-only usages: (a) bedrock file-retrieve s3-bucket-host-injection tests (#7307) embed it in a URL-encoded s3 bucket path to assert Bifrost's own gate rejects it with 400 'invalid s3 bucket name' before any upstream dial; (b) the mcp oauth2 server discovery-document test sends it only as a spoofed Host header against Bifrost's own baseUrl, asserting the returned issuer/token_endpoint/jwks_uri do NOT echo it back - never a target Bifrost or the harness dials",
    "auth.example.invalid": "reserved .example.invalid; authorize_url/token_url in the mcp-oauth-cimd-client [EXPECT-400] toggle tests - both cases assert the POST /api/mcp/client save itself is refused (400, before any authorization server could be reached), same mechanism as idp.example",
    "mcp.example.invalid": "reserved .example.invalid; connection_string in the same mcp-oauth-cimd-client [EXPECT-400] toggle tests as auth.example.invalid above - the save is refused before the MCP server is ever contacted",
    "other-mcp.invalid": "reserved .invalid TLD; mcp-oauth-resource-match [PREVIEW] test asserts the returned authorize_url does NOT include it - the row's prerequisite local fixture (mcp-oauth-resource-fixture.mjs, port 8799) is never started by the harness runners, so this row does not even execute in CI, let alone dial a real host",
    "inspector.example.com": "used only as the value of an Origin request header on an OPTIONS CORS-preflight probe against Bifrost's own baseUrl - a string Bifrost's CORS logic compares, never a URL anyone dials",
    "s3.us-east-1.amazonaws.com": "base_url value in a sigv4-relay create/delete config round trip (relays-e2e) - the folder description states plainly 'Management API only, the upstream is never called'; only Bifrost's own /api/relays CRUD is exercised, no request is proxied through the relay in this test",
    # Documentation links inside descriptions and comments.
    "anthropic.com": "doc link in a folder description",
    "www.anthropic.com": "doc link in a folder description",
    "platform.claude.com": "doc link in a folder description",
    "code.claude.com": "doc link in a test case description (auto-mode-classifier-billing)",
    "developers.openai.com": "doc link in a folder description",
    "docs.aws.amazon.com": "doc link in a folder description",
    "docs.x.ai": "doc link in a folder description",
    "schema.getpostman.com": "Postman collection schema declaration, not fetched at run time",
    # Identifiers that merely look like endpoints.
    "s3.amazonaws.com": "XML namespace URI in a ListBucketResult document; namespaces are names, not fetches",
    "www.googleapis.com": "the cloud-platform OAuth SCOPE string; the token exchange itself goes to oauth2.googleapis.com",
    "www.ietf.org": "WebRTC SDP a=extmap identifier URI (draft-holmer-rmcat-transport-wide-cc-extensions-01) - an RFC-style namespace identifier, not a URL anyone fetches, same pattern as s3.amazonaws.com",
    "www.w3.org": "SSML xmlns namespace URI embedded in the <speak> request bodies sent to Azure's speech-synthesis endpoint - an XML namespace identifier, not a URL anyone fetches, same pattern as s3.amazonaws.com",
}

HOST_RE = re.compile(r"https?://([A-Za-z0-9._-]+\.[A-Za-z]{2,})")
# {{var}} placeholders resolve at run time from secrets; the concrete hosts they
# expand to (regional Bedrock/Vertex endpoints) cannot be known statically, so
# they are out of scope here and stay a manual review item.
TEMPLATED = re.compile(r"\{\{")


def hosts_in(path):
    if not os.path.exists(path):
        return set()
    with open(path, encoding="utf-8") as fh:
        raw = fh.read()
    found = set()
    for line in raw.splitlines():
        if TEMPLATED.search(line):
            # Strip only URLs whose AUTHORITY is templated -- those expand to hosts that
            # cannot be known statically. A literal host with a templated PATH
            # ("https://files.example.com/{{file_id}}") still dials a knowable host and
            # must stay in the census, or the validator passes a blocked-egress job that
            # harden-runner will then kill at the request. Excluding "/" from the span
            # before the placeholder is what draws that line.
            line = re.sub(r"https?://[^\s\"'/]*\{\{[^\s\"']*", " ", line)
        for m in HOST_RE.finditer(line):
            found.add(m.group(1))
    return found


# Provider endpoints Bifrost builds in Go rather than reading from config, which
# is why scanning the seeded config alone is not enough: discoveryengine
# (the Vertex reranker backend) is assembled in core/providers/vertex and appears
# in no config file, so it was missing from two workflows' allowlists at once.
# Restricted to the cloud-provider domains whose hostnames are constructed this
# way; anything else in that source tree is a doc link, not a dial.
GO_HOST_SUFFIXES = (".googleapis.com", ".amazonaws.com", ".api.aws")


def hosts_in_go(root):
    found = set()
    providers_dir = os.path.join(root, "core", "providers")
    for dirpath, _, filenames in os.walk(providers_dir):
        for name in filenames:
            if not name.endswith(".go") or name.endswith("_test.go"):
                continue
            with open(os.path.join(dirpath, name), encoding="utf-8", errors="ignore") as fh:
                text = fh.read()
            for m in HOST_RE.finditer(text):
                host = m.group(1)
                if host.endswith(GO_HOST_SUFFIXES):
                    found.add(host)
    return found


required = hosts_in(COLLECTION) | hosts_in(PROVIDER_CONFIG) | hosts_in_go(REPO_ROOT)
required -= set(NOT_DIALLED_HOSTS)
required = {h for h in required if h not in ("localhost",)}

failures = []
checked = 0

for path in sys.argv[1:]:
    with open(path, encoding="utf-8") as fh:
        doc = yaml.safe_load(fh) or {}

    for job_name, job in (doc.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        steps = job.get("steps") or []

        runs_harness = any(
            any(marker in (step.get("run") or "") for marker in HARNESS_MARKERS)
            for step in steps
            if isinstance(step, dict)
        )
        if not runs_harness:
            continue

        harden = next(
            (
                step
                for step in steps
                if isinstance(step, dict)
                and "step-security/harden-runner" in (step.get("uses") or "")
            ),
            None,
        )
        if harden is None:
            continue

        with_block = harden.get("with") or {}
        if str(with_block.get("egress-policy", "")).strip() != "block":
            continue

        checked += 1
        allowed = {
            entry.rsplit(":", 1)[0]
            for entry in (with_block.get("allowed-endpoints") or "").split()
        }
        missing = sorted(h for h in required if h not in allowed)
        if missing:
            failures.append((path, job_name, missing))

for path, job_name, missing in failures:
    print(
        f"FAIL {path} :: job '{job_name}' runs the provider harness under "
        f"egress-policy: block but omits {', '.join(missing)}",
        file=sys.stderr,
    )

if failures:
    print(
        "\nEach host above appears in the provider harness collection or its seeded\n"
        "provider config. Either add it to that job's allowed-endpoints, or - if the\n"
        "provider dereferences it server-side and the runner never dials it - add it to\n"
        "NOT_DIALLED_HOSTS in this script with the reason.",
        file=sys.stderr,
    )
    sys.exit(1)

if checked == 0:
    print("OK: no harness-running blocked-egress jobs found.")
else:
    print(
        f"OK: all {checked} harness-running blocked-egress job(s) allowlist the "
        f"{len(required)} host(s) the harness dials."
    )
PY

# ---------------------------------------------------------------------------
# 3. Provider default hosts for the keys a job is given
# ---------------------------------------------------------------------------
#
# Guard 2 only sees hosts that appear in the harness collection or config. Most
# providers' hosts appear in neither: they are the default BaseURL in Go. A job
# that passes RUNWAY_API_KEY under egress-policy: block runs Runway's live tests,
# and if api.dev.runwayml.com is not allowlisted those tests fail as a
# connection error that reads like a flaky provider.
#
# So: for every blocked-egress job that runs the per-provider live suites (those
# run every provider whose key is set), each <PROVIDER>_API_KEY / _TOKEN /
# _CREDENTIALS it sets is matched to core/providers/<provider>/, and every host
# on a non-comment BaseURL line there must be allowlisted or exempted below.
# Jobs that hold keys for other reasons (release, plugin, docker-image tests)
# dial only the providers they use, so they are not held to every default host.

python3 - "${FILES[@]}" <<'PY'
import os
import re
import sys
import yaml

REPO_ROOT = os.environ["REPO_ROOT"]
PROVIDERS_DIR = os.path.join(REPO_ROOT, "core", "providers")
if not os.path.isdir(PROVIDERS_DIR):
    print(f"::error::egress check input not found: {PROVIDERS_DIR}", file=sys.stderr)
    sys.exit(1)

# Hosts on a BaseURL line that the test suites never dial, each with the reason.
NOT_DIALLED_PROVIDER_HOSTS = {
    "iamcredentials.googleapis.com": "vertex AWS-federation token exchange; CI authenticates with a service-account key",
}

# Scripts that run core/providers' live suites for every key that is set.
PROVIDER_SUITE_MARKERS = ("run-tests.sh", "test-core.sh")

KEY_RE = re.compile(r"^([A-Z0-9_]+?)_(?:API_KEY|API_TOKEN|TOKEN|CREDENTIALS)$")
HOST_RE = re.compile(r"\"https://([A-Za-z0-9.-]+\.[A-Za-z]{2,})")


def provider_hosts(provider):
    hosts = set()
    pdir = os.path.join(PROVIDERS_DIR, provider)
    for name in os.listdir(pdir):
        if not name.endswith(".go") or name.endswith("_test.go"):
            continue
        with open(os.path.join(pdir, name), encoding="utf-8", errors="ignore") as fh:
            for line in fh:
                if line.lstrip().startswith("//") or "baseurl" not in line.lower():
                    continue
                hosts.update(HOST_RE.findall(line))
    return hosts - set(NOT_DIALLED_PROVIDER_HOSTS)


providers = {d for d in os.listdir(PROVIDERS_DIR) if os.path.isdir(os.path.join(PROVIDERS_DIR, d))}
failures = []
checked = 0

for path in sys.argv[1:]:
    with open(path, encoding="utf-8") as fh:
        doc = yaml.safe_load(fh) or {}

    for job_name, job in (doc.get("jobs") or {}).items():
        if not isinstance(job, dict):
            continue
        steps = [s for s in (job.get("steps") or []) if isinstance(s, dict)]
        if not any(m in (s.get("run") or "") for s in steps for m in PROVIDER_SUITE_MARKERS):
            continue
        harden = next((s for s in steps if "step-security/harden-runner" in (s.get("uses") or "")), None)
        if harden is None:
            continue
        with_block = harden.get("with") or {}
        if str(with_block.get("egress-policy", "")).strip() != "block":
            continue

        env_names = set(job.get("env") or {})
        for s in steps:
            env_names.update(s.get("env") or {})
        wanted = {}
        for name in env_names:
            m = KEY_RE.match(name)
            if not m:
                continue
            provider = m.group(1).lower().replace("_", "")
            if provider in providers:
                wanted[name] = provider_hosts(provider)
        if not wanted:
            continue

        checked += 1
        allowed = {e.rsplit(":", 1)[0] for e in (with_block.get("allowed-endpoints") or "").split()}
        missing = sorted({f"{h} ({k})" for k, hs in wanted.items() for h in hs if h not in allowed})
        if missing:
            failures.append((path, job_name, missing))

for path, job_name, missing in failures:
    print(f"FAIL {path} :: job '{job_name}' runs the provider suites under egress-policy: block "
          f"but omits {', '.join(missing)}", file=sys.stderr)

if failures:
    print("\nEach host above is a provider's default BaseURL in core/providers. Add it to\n"
          "that job's allowed-endpoints, or to NOT_DIALLED_PROVIDER_HOSTS with the reason.",
          file=sys.stderr)
    sys.exit(1)

print(f"OK: all {checked} provider-suite blocked-egress job(s) allowlist their providers' default hosts.")
PY
