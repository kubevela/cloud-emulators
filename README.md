# cloud-emulators

Container images and manifests for running cloud emulators inside a test cluster, so
that controllers such as ACK can be pointed at them. KubeVela uses them to test cloud
component definitions without a cloud account.

## AWS Emulator

`ghcr.io/kubevela/aws-emulator:<version>` is an **unofficial** build of
[AWS Emulator](https://github.com/aws/aws-emulator). AWS publishes Linux packages but no
container image. The tag is the AWS Emulator version inside, for example `1.0.530`.
Report emulator bugs to the [AWS Emulator repository](https://github.com/aws/aws-emulator/issues),
not here.

The image differs from running AWS's package directly in three ways:

| | AWS's package | This image |
|---|---|---|
| Endpoint | HTTPS on `127.0.0.1:9297`, certificate remade at every start | plain HTTP on port `4566` |
| Access keys | only the keys generated for that run | any keys (`--auth off`) |
| Telemetry | on | off |

A controller in another pod cannot trust a certificate that changes at every restart, or
use keys that do. So the container starts with a small launcher
([`aws/launcher`](aws/launcher/main.go)) that runs the emulator, accepts plain HTTP and
forwards it to the emulator over TLS, checking the certificate of that run. When the
emulator stops, the launcher stops, and the container restarts as a whole.

The emulator does not check IAM policies the way AWS does, keeps everything in memory,
and has one account (`000000000000`). With signing checks off, anyone who can reach port
4566 can use it.

### Where to use it

**Only on a cluster that is thrown away after the tests**: k3d or kind on a laptop, or
one created inside a CI job. There the controller is installed pointing at the emulator
from the start, and the whole cluster is deleted at the end.

**Not on a real cluster such as EKS, and never by repointing a controller that manages
real AWS resources.** An ACK controller takes its endpoint when it is installed. Turning
it toward the emulator and back means reinstalling it, and in between it holds resources
it believes it created in the other place. Controllers that talk to AWS stay on AWS;
emulator tests get their own disposable cluster.

### Run it

```shell
docker run --rm -p 127.0.0.1:4566:4566 ghcr.io/kubevela/aws-emulator:1.0.530

export AWS_ENDPOINT_URL=http://127.0.0.1:4566
export AWS_ACCESS_KEY_ID=000000000000 AWS_SECRET_ACCESS_KEY=anything AWS_REGION=us-west-2
aws dynamodb list-tables
```

In a cluster, apply the manifest attached to a
[release](https://github.com/kubevela/cloud-emulators/releases); it names the image by
digest:

```shell
kubectl apply -f https://github.com/kubevela/cloud-emulators/releases/download/aws-emulator-v1.0.530/aws-emulator-1.0.530.yaml
```

The endpoint is `http://aws-emulator.aws-emulator.svc.cluster.local:4566`, reachable
from any namespace of that cluster and from nowhere else.

For an ACK controller, set these chart values and give it a credentials Secret with any
keys:

```yaml
aws:
  region: us-west-2
  endpoint_url: http://aws-emulator.aws-emulator.svc.cluster.local:4566
  allow_unsafe_aws_endpoint_urls: true
```

### Settings

| Variable | Default | Meaning |
|---|---|---|
| `EMULATOR_HTTP_PORT` | `4566` | Port the launcher listens on |
| `EMULATOR_AUTH` | `off` | `on` makes the emulator accept only the keys it generated, in `~/.aws/emulator/9297/emulator_credentials` inside the container |
| `EMULATOR_TELEMETRY` | `off` | `on` sends AWS anonymous usage data |

Arguments after the image name go to `aws-emulator`, for example `--idle-timeout 3600`.

### Which version, and how well it matches AWS

AWS releases almost daily and scores every release against the live service. Pin a
version in tests, and read that release's fidelity report before relying on an API. It is
in the image at `/opt/aws-emulator/release-info/scores.json.gz`, and on the
[AWS Emulator release](https://github.com/aws/aws-emulator/releases).

A workflow here checks every six hours. AWS serves only its newest package, so a version
is published once, when it is the newest, and is never rebuilt or overwritten. A version
that came and went between two checks has no image.

### Security

What the image is made of:

- **No shell, no package manager.** The final image is
  [distroless](https://github.com/GoogleContainerTools/distroless): the C library, CA
  certificates, the emulator and the launcher. The launcher is a static Go binary with no
  dependencies outside the standard library.
- **Non-root.** It runs as user `65532`. The manifest also sets a read-only root
  filesystem, drops every Linux capability, uses the default seccomp profile, mounts no
  Kubernetes API token, and adds a NetworkPolicy that lets the emulator open no
  connections of its own. The namespace enforces the `restricted` Pod Security Standard.
- **Pinned inputs.** Base images are pinned by digest and workflow actions by commit;
  Dependabot proposes updates to both.

What happens before an image is pushed:

- The launcher's tests, the [hadolint](https://github.com/hadolint/hadolint) Dockerfile
  check, and a smoke test that runs the image under the manifest's restrictions.
- A [Trivy](https://github.com/aquasecurity/trivy) scan. A fixable CRITICAL or HIGH
  finding in the operating-system layer stops the release. Findings in the emulator
  binary itself are reported but do not block, since only AWS can fix them.
- A Trivy check of the manifest.

What a pushed image carries:

- A [cosign](https://github.com/sigstore/cosign) signature made with the workflow's own
  identity, with no long-lived key.
- A build provenance attestation and an SBOM.
- The SHA-256 checksums of AWS's packages as downloaded, at
  `/opt/aws-emulator/release-info/SHA256SUMS` and on the release. AWS does not sign its
  packages, so this records exactly what was packaged; it cannot prove where AWS's
  package came from.

Verify an image before using it:

```shell
cosign verify ghcr.io/kubevela/aws-emulator:1.0.530 \
  --certificate-identity-regexp '^https://github.com/kubevela/cloud-emulators/\.github/workflows/aws-emulator-image\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

gh attestation verify oci://ghcr.io/kubevela/aws-emulator:1.0.530 --repo kubevela/cloud-emulators
```

Rules, on top of "disposable clusters only" above:

- **Test data only.** Never store real data or real credentials in it. State is in
  memory and readable by every client.
- **Never expose it.** No Ingress, no LoadBalancer, no public port-forward.
- **Pin and verify.** Deploy by digest from a release manifest, verify the signature, and
  move to a new version on purpose.

Limits to know about:

- AWS says the emulator's code is written by AI agents and not reviewed by people. This
  image repackages it and does not audit it.
- Anyone who can reach the emulator can make it run Python or Node function code
  (Lambda). The emulator runs that code in its own built-in interpreters; in version
  1.0.523 they refused host files, child processes and network connections, and uploaded
  binaries (custom runtimes) did not run. AWS still documents function code as not
  isolated, so the pod's own restrictions are the boundary to rely on.

### Build it yourself

```shell
aws/build.sh local                 # downloads AWS's packages, builds aws-emulator:local
docker run -d -p 127.0.0.1:4566:4566 aws-emulator:local
aws/smoke-test.sh                  # the calls an ACK DynamoDB controller makes
```

## Test library

[`testkit`](testkit) is a Go package for test suites that run a cloud controller against
the emulator. It has no test framework of its own: whatever can fail returns an error, so
it works with Ginkgo, the standard `testing` package or a plain program.

```go
import "github.com/kubevela/cloud-emulators/testkit"

func setUp(ctx context.Context) (*testkit.Cluster, *testkit.Run, string, func(), error) {
	cluster, err := testkit.Connect() // refuses anything but a local k3d or kind cluster
	if err != nil {
		return nil, nil, "", nil, err
	}
	// Deploys the emulator only when it is missing.
	if err := cluster.EnsureAWSEmulator(ctx, "1.0.533"); err != nil {
		return nil, nil, "", nil, err
	}
	// Stop here if the controller talks to AWS: nothing below may run then.
	if err := cluster.RequireACKOnEmulator(ctx, "dynamodb"); err != nil {
		return nil, nil, "", nil, err
	}
	run, err := cluster.NewRun(ctx, "dynamodb") // a namespace and a name prefix of its own
	if err != nil {
		return nil, nil, "", nil, err
	}
	// An endpoint for an AWS client in the test process. Call stop when the tests end.
	endpoint, stop, err := cluster.ForwardAWSEmulator(ctx)
	if err != nil {
		return nil, nil, "", nil, err
	}
	return cluster, run, endpoint, stop, nil
}

// After the tests: stop(), then
// cluster.EndRun(ctx, run, owners, dependents) // waits for the controller to clean up
```

What it takes care of:

- **Only disposable clusters.** `Connect` returns an error unless the kubeconfig context
  is named like a k3d or kind cluster and its API server is on this machine (loopback,
  `localhost`, `host.docker.internal`, or the cluster's own node container). The name
  alone is not trusted.
- **One emulator per cluster.** A controller has one endpoint, so tests share the
  emulator. `EnsureAWSEmulator` uses the one that is running and applies a release
  manifest only when its Deployment or its Service is missing.
- **Never real AWS.** `RequireACKOnEmulator` fails when the service's ACK controller is
  missing, not running, or configured with any endpoint other than the emulator's. When
  the controller is not installed, the error carries the install command;
  `ACKInstallCommand` returns the same command for a given chart version.
- **Runs that do not collide.** `NewRun` gives each run a namespace and an ID to prefix
  names in the cloud service with. `EndRun` deletes what the run applied, waits until the
  controller has removed its resources from the service, then deletes the namespace.
- **Reading ACK status correctly.** `ACKReady` is true only when `ACK.ResourceSynced` is
  `True` and `ACK.Terminal` is not. A resource the controller gave up on keeps its last
  Synced value.

## Layout

```
aws/Dockerfile            the image
aws/launcher/             starts the emulator and forwards plain HTTP to it
aws/build.sh              downloads AWS's packages; builds locally
aws/smoke-test.sh         checks an endpoint as a controller would use it
aws/deploy/               manifest for a cluster
testkit/                  Go package that prepares a disposable cluster for emulator tests
.github/workflows/        tests, scans, publishes and signs an image per AWS Emulator version
```

## License

The files in this repository are under the Apache License 2.0. AWS Emulator itself is
released by AWS under CC0 1.0, with third-party code under the licenses listed in
`/opt/aws-emulator/THIRD_PARTY_LICENSES` inside the image.
