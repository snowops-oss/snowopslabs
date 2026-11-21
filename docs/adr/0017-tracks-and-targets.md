# ADR 0017: Labs beyond Kubernetes: tracks and targets

**Status:** Accepted
**Date:** 2026-11-21

## Context

SnowOps Labs is defined as a Kubernetes platform-engineering simulator.
[ADR-0001](0001-cut-cloud-and-commercial-scope.md) removed the AKS, EKS and
GKE runtimes because nobody ever ran them against a real account, and they
took effort the core loop needed.

The core loop is now solid: thirteen scenarios, the incident library, a
learning path, challenges and team mode, each reviewed on a live lab. The
people it serves do more than run Kubernetes. Platform and DevOps engineers
also own pipelines, infrastructure as code and cloud accounts, and those break
in ways that need the same practice: a cache key that ships the wrong build, a
state lock nobody released, a plan that would destroy a database.

Adding them carelessly would repeat v1: many environments, none verified, and
a product that is harder to explain. Three things have to be settled before
any of that work starts:

- how new environments are grouped so the catalog stays easy to read,
- how the engine reaches something other than the one cluster, and
- how a learner's own accounts and credentials are handled.

Vendor terms also move. In 2026 LocalStack started requiring an account and
auth token, and MinIO stopped publishing community images and archived its
repository. Content built on a tool whose terms or maintenance change breaks
for everyone, so contributors need a rule before they pick one.

## Decision

### Tracks group the catalog

A **track** is a group of content about one area of platform work:
Kubernetes, CI/CD, infrastructure as code, and later cloud providers. Each
track has a free core of scenarios and incidents, a learning path and at least
one challenge. Kubernetes is the first track and stays the base of every lab,
because the other tools run in or deploy to the cluster.

Work on a new track starts only when all five of these hold:

1. **The free path costs nothing.** It runs on open-source tools in the lab,
   or on a free tier the learner can use without a card being charged.
2. **The licences allow it.** Every tool it installs meets the rule in
   [Tool licences](#tool-licences) below.
3. **Checks grade it from outside.** An API, a CLI or a metric answers the
   check. Nothing is graded on the learner's word.
4. **Someone asked for it**, in an issue or a discussion, and the previous
   track is finished and used.
5. **Someone maintains it**: a maintainer or a contributor who owns it.

The order is CI/CD first (a Git forge with an Actions runner in the cluster,
with an optional mode on the learner's own GitHub repository), then
infrastructure as code (OpenTofu with an in-cluster S3 store), then cloud
providers. The ADR-0001 rule stays for anything that touches a real account:
content that runs against a cloud or SaaS account merges only after it has run
end to end against a real sandbox account. Content that runs only on an
open-source emulator in the lab is tested like any other local content.

The catalog, CLI and UI filter by track. Tracks add no new commands.

### Targets are an engine word, not a learner word

A lab is a set of named **targets**. The Kubernetes cluster is the default
target, named `k8s`, and k3d, kind and incluster become its providers. A Git
forge, a tofu state store or a connected GitHub account are other targets.

- A target provider has the same contract shape as a runtime profile today:
  up, down, status, and an outputs file that later steps read.
- Every run gets its context passed in explicitly (`KUBECONFIG`, tokens,
  endpoints). Nothing relies on the shell's current kube-context.
- Content says what it needs (`requires: [k8s, git-forge]`). Components,
  checks and faults take an optional `target:` that defaults to `k8s`, so
  every existing scenario, incident and challenge keeps working unchanged.

Managed clusters (AKS, EKS, GKE) as the lab's own runtime stay out of scope.
The runtime profile contract still allows one later, verified against a real
account, as ADR-0001 says. Cloud providers arrive as tracks instead.

Learners never need the word. They still work in a lab through the same four
loops, `scenario up` brings up whatever the scenario requires, and the CLI
asks them to connect an account only when a scenario needs one.

### Outside accounts stay on the learner's machine

- `labctl connect <provider>` stores a token in the operating system's
  keychain. labctl passes it only to the runs that need it and redacts it
  from recorded run output. It is never sent anywhere else.
- A scenario or fault that changes a real account runs only against a
  sandbox: a throwaway GitHub repository or organisation, or a sandbox cloud
  account. Preflight refuses accounts that look like production, tags what it
  creates, shows a cost estimate before a cloud scenario starts, and tears
  down in reverse order.

### Tool licences

A tool may be installed by content only if its licence or terms allow:

- running it unmodified in the free local lab, at no cost to the learner, and
- running it unmodified inside a hosted lab
  ([ADR-0016](0016-hosted-labs-long-term.md)).

OSI-approved licences meet both. Source-available licences (BSL, SSPL,
Elastic) and free tiers with their own terms need the terms read and the
result written in the pull request that adds the tool. A tool whose community
builds have stopped is not added. Trademarks are used only to name the tool.

This rule applies to new tools from now on. It is why the IaC track uses
OpenTofu (MPL-2.0) rather than Terraform (BSL), and why new S3 storage uses a
maintained store rather than MinIO. Vault (BSL 1.1) stays in the secrets
scenario's local lab, and OpenBao (MPL-2.0) is the candidate provider wherever
BSL terms do not fit.

## Alternatives considered

| Alternative | Why rejected |
|---|---|
| Stay Kubernetes-only | Leaves out the pipeline and IaC work platform teams practise least and break most |
| Add every environment with a free tier at once | The v1 mistake again: breadth nobody verifies, and a catalog nobody can read |
| Bring back cloud runtimes first | Costs learners money, depends on vendor free tiers that keep shrinking, and needs a sandbox account to verify each change |
| Expose targets to learners as a concept | Adds a word to the first run that only authors need |
| Learners send us credentials for their environments | Makes the project hold secrets it cannot protect, and makes us liable for faults run in someone's account |
| No licence rule, judge each tool in review | Contributors find out after they have built the content |

## Consequences

- **Easier:** CI/CD and IaC content can be built on the existing engine, run
  offline on a laptop, and graded like everything else. Contributors know
  before they start whether a tool is allowed.
- **Harder:** the engine needs groundwork before the first new track: named
  targets, explicit context for every run, `connect` with the keychain, and
  track metadata in the catalog. Each lands behind tests showing existing
  content is unchanged.
- **Cost:** every new track adds a platform component, its memory budget and
  its own review. A track that cannot meet the five rules waits.
- **Product text:** [PRODUCT.md](../PRODUCT.md) describes the product by
  track from now on. The README and the tagline say "Kubernetes" until a
  second track ships.
- ADR-0001 still stands for the commercial surfaces and for the v1 code it
  deleted. This record replaces only its decision that the lab covers local
  Kubernetes and nothing else.
