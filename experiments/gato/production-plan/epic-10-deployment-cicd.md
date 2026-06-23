# Epic 10: Deployment & CI/CD

## Business Meaning

Gato must ship reliably to production without human intervention. This means a reproducible Docker build that includes all native dependencies (CGO for ONNX and Opus), a Cloud Build pipeline that pushes to Cloud Run on merge, and a load test harness that gives engineers confidence before each release.

Without this epic, Gato can only run on a developer's laptop. With it, every merge to `main` produces a tested, deployed binary running in the cloud.

---

## Background

Gato has two CGO dependencies not present in the switchboard:
1. **ONNX Runtime** (`onnxruntime-go`) — required for Silero VAD inference
2. **Opus codec** (`hraban/opus`) — requires `libopus-dev` and `pkg-config`

The switchboard's Dockerfile (`switchboard/Dockerfile`) uses `CGO_ENABLED=0`. Gato cannot. The build image must include both native libraries, and the runtime image must include the shared libraries.

Reference:
- `switchboard/Dockerfile` — multi-stage build pattern
- `switchboard/cloudbuild-switchboard.yaml` — Cloud Build pattern
- `switchboard/Makefile` — make targets
- `experiments/gato/experiments/exp-009/` — performance harness for 10-session load

---

## Tasks

### Task 10.1 — Dockerfile with CGO dependencies

Create `gato/Dockerfile`. Multi-stage build:

**Stage 1: Builder** (`golang:1.23-bookworm`)

Debian (not Alpine) because ONNX Runtime's pre-built libraries are glibc-linked, not musl-compatible.

```dockerfile
FROM golang:1.23-bookworm AS builder

# Install native deps for Opus and ONNX
RUN apt-get update && apt-get install -y --no-install-recommends \
    libopus-dev \
    pkg-config \
    && rm -rf /var/lib/apt/lists/*

# Install ONNX Runtime (pre-built shared library)
ARG ONNXRUNTIME_VERSION=1.17.3
RUN curl -L "https://github.com/microsoft/onnxruntime/releases/download/v${ONNXRUNTIME_VERSION}/onnxruntime-linux-x64-${ONNXRUNTIME_VERSION}.tgz" \
    | tar -C /usr/local -xz --strip-components=1

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    CGO_CFLAGS="-I/usr/local/include/onnxruntime" \
    CGO_LDFLAGS="-L/usr/local/lib -lonnxruntime" \
    go build -ldflags="-X main.version=${VERSION}" \
    -o bin/gato ./cmd/gato
```

**Stage 2: Runtime** (`debian:bookworm-slim`)

```dockerfile
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    libopus0 \
    ca-certificates \
    wget \
    && rm -rf /var/lib/apt/lists/*

# Copy ONNX Runtime shared library from builder
COPY --from=builder /usr/local/lib/libonnxruntime.so.* /usr/local/lib/
RUN ldconfig

# Copy the Silero VAD model
COPY --from=builder /build/models/silero_vad_v5.onnx /opt/gato/silero_vad_v5.onnx

# Non-root user
RUN useradd -r -s /bin/false gato
COPY --from=builder /build/bin/gato /gato
RUN chown gato:gato /gato

USER gato

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
    CMD wget -qO- http://localhost:8390/health || exit 1

EXPOSE 8390
EXPOSE 8391  # pprof / debug (internal)

ENTRYPOINT ["/gato"]
```

**Key decisions:**
- `debian:bookworm-slim` (not `alpine`) for glibc compatibility with ONNX Runtime
- ONNX Runtime version pinned; downloaded from GitHub releases (reproducible)
- VAD model bundled in the image (no runtime download, no external dependency at startup)
- Non-root user for security

### Task 10.2 — Makefile

Create `gato/Makefile`. Follow `switchboard/Makefile` structure:

```makefile
BINARY      := gato
VERSION     ?= $(shell git describe --tags --always --dirty)
IMAGE       := asia-south1-docker.pkg.dev/voqal-cloud-dev/voqalcloud-images/gato

build:
	CGO_ENABLED=1 go build -ldflags="-X main.version=$(VERSION)" -o bin/$(BINARY) ./cmd/gato

proto:
	cp ../switchboard/internal/proto/switchboard.pb.go internal/proto/

test:
	go test -race ./internal/... ./cmd/... ./sdk/...

test-integration:
	go test -v -timeout 120s ./tests/integration/...

test-load:
	go test -v -timeout 600s -run TestLoad ./tests/load/...

docker-build:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) -t $(IMAGE):latest .

docker-push:
	docker push $(IMAGE):$(VERSION)
	docker push $(IMAGE):latest

lint:
	go vet ./...
	golangci-lint run

.PHONY: build proto test test-integration test-load docker-build docker-push lint
```

### Task 10.3 — Cloud Build pipeline

Create `gato/cloudbuild-gato.yaml`. Follow `switchboard/cloudbuild-switchboard.yaml`:

```yaml
steps:
  - name: 'golang:1.23-bookworm'
    entrypoint: bash
    args: ['-c', 'cd gato && make test']
    id: unit-tests

  - name: 'gcr.io/cloud-builders/docker'
    args:
      - build
      - --build-arg
      - VERSION=$SHORT_SHA
      - -t
      - asia-south1-docker.pkg.dev/voqal-cloud-dev/voqalcloud-images/gato:$SHORT_SHA
      - -t
      - asia-south1-docker.pkg.dev/voqal-cloud-dev/voqalcloud-images/gato:latest
      - gato/
    waitFor: [unit-tests]
    id: docker-build

  - name: 'gcr.io/cloud-builders/docker'
    args: [push, asia-south1-docker.pkg.dev/voqal-cloud-dev/voqalcloud-images/gato:$SHORT_SHA]
    waitFor: [docker-build]

  - name: 'gcr.io/google.com/cloudsdktool/cloud-sdk'
    entrypoint: gcloud
    args:
      - run
      - deploy
      - voqalcloud-gato
      - --image=asia-south1-docker.pkg.dev/voqal-cloud-dev/voqalcloud-images/gato:$SHORT_SHA
      - --region=asia-south1
      - --platform=managed
      - --no-allow-unauthenticated
      - --service-account=gato@voqal-cloud-dev.iam.gserviceaccount.com
    waitFor: [docker-build]

options:
  machineType: E2_HIGHCPU_8
  logging: CLOUD_LOGGING_ONLY
```

### Task 10.4 — Cloud Run configuration

Cloud Run deployment requirements for Gato differ from the stateless Switchboard:

**CPU**: always allocated (not only during request handling) — Gato has continuous background goroutines (VAD, audio output, ping loop). Set `--cpu-always-allocated`.

**Concurrency**: `--concurrency=50` (Cloud Run's HTTP concurrency setting). Gato manages its own session concurrency internally; Cloud Run should funnel all traffic to one instance until it's at capacity.

**Min instances**: 1 — the VAD model loading takes ~2s; cold starts are unacceptable for a real-time voice service.

**Memory**: `--memory=2Gi` — ONNX Runtime + model + 20 concurrent sessions with audio buffers.

**CPU**: `--cpu=2` — VAD ONNX inference is CPU-bound; 2 vCPUs needed for 20 sessions.

**Service account**: dedicated `gato@<project>.iam.gserviceaccount.com` with Workload Identity bindings for:
- `roles/speech.client` (Google STT)
- `roles/texttospeech.client` (Google TTS)

### Task 10.5 — Load test harness

Create `gato/tests/load/load_test.go`. This is a Go test file that starts Gato in-process and drives N concurrent sessions.

The harness from EXP-009 (`experiments/gato/experiments/exp-009/`) ran 10 sessions with mock STT/TTS. The production load test adds:

- Real Silero VAD (CGO) to measure actual inference cost
- Stub STT/TTS (no network) to isolate the CPU path
- `GOMAXPROCS=2` to simulate Cloud Run's 2-vCPU environment

```go
func TestLoad_10Sessions(t *testing.T) {
    gato := startGatoInProcess(t, Config{MaxSessions: 10})
    
    var wg sync.WaitGroup
    for i := 0; i < 10; i++ {
        wg.Add(1)
        go func(id int) {
            defer wg.Done()
            runFakeSession(t, gato, 60*time.Second, speechWAV)
        }(i)
    }
    wg.Wait()
    
    metrics := gato.Metrics()
    assert.Less(t, metrics.VADInferP99, 5*time.Millisecond)
    assert.Equal(t, 0, int(runtime.NumGoroutine() - baselineGoroutines))
}
```

Run at L1 (1 session), L2 (5 sessions), L3 (10 sessions), L4 (20 sessions). For L4, disable `-race` (race detector has 5-10× overhead).

Pass criteria (from EXP-009):
- VAD p99 < 5ms at N=10
- Zero goroutine growth across the test duration
- All sessions complete cleanly (no panics, no errors)

### Task 10.6 — Integration test harness with fake switchboard

Create `gato/tests/integration/harness.go` with:

```go
// Harness starts a fake switchboard + real Gato binary, wired together over real TCP.
type Harness struct {
    Switchboard *FakeSwitchboard
    GatoURL     string
    // ...
}

func NewHarness(t *testing.T) *Harness
func (h *Harness) SimulateSession(sessionID string, audio []byte) SessionResult
func (h *Harness) Teardown()
```

`FakeSwitchboard` is a real TCP server that speaks the Switchboard node protocol (protobuf frames). It accepts Gato's registration and can inject probes, signals, and releases. Mirror the pattern from `switchboard/tests/integration/`.

`SessionResult` holds the TTS audio received by the fake browser, the transcripts logged, and whether the session ended cleanly.

### Task 10.7 — Canary deployment

Document the canary procedure in `gato/docs/deployment.md`:

1. Deploy new version to `voqalcloud-gato-canary` (separate Cloud Run service, same Switchboard)
2. Set Switchboard's `K_PROBE=3` to distribute probes across both nodes
3. Monitor `gato_sessions_total{outcome="failed"}` on the canary for 30 minutes
4. If no failures: promote to `voqalcloud-gato` (full deployment)
5. If failures: SIGTERM the canary; it drains and exits; full deployment stays on old version

This requires no special tooling — just two Cloud Run services both registered to the same Switchboard. The Switchboard's load-balancing across nodes is the canary mechanism.

---

## Definition of Done

- [ ] `docker build gato/` succeeds on an x86-64 Linux host with no pre-installed dependencies
- [ ] The built image starts, passes `/health`, and accepts a real WebRTC session
- [ ] `make test` runs in CI (GitHub Actions or Cloud Build) without any manual setup
- [ ] `make test-load` at L3 (10 sessions) passes: VAD p99 < 5ms, zero goroutine growth
- [ ] Cloud Build pipeline deploys to Cloud Run on push to `main`
- [ ] SIGTERM drains cleanly in the Cloud Run environment (Cloud Run sends SIGTERM 10s before force-kill)

---

## Verification

### Unit Tests

- `TestDockerfile_BuildOutputExists`: not a Go test — CI step. `docker build` must exit 0.
- `TestVersion_EmbeddedInBinary`: `go build -ldflags="-X main.version=test-1.2.3"`; run binary; assert `/health` returns `"version": "test-1.2.3"`

### Integration Tests (CI)

Run `make test-integration` in Cloud Build using the integration harness:
- `TestHarness_ProbeAckRoundTrip`: fake switchboard probes real Gato; assert Ack received
- `TestHarness_SessionEndToEnd`: fake switchboard + fake browser + real VAD/STT/TTS stubs; assert transcript delivered

### Load Test (pre-release gate)

Run `make test-load` as a required CI check before promoting a canary:
- L1: 1 session × 60s → pass
- L2: 5 sessions × 60s → pass  
- L3: 10 sessions × 60s → VAD p99 < 5ms, no goroutine leak → **required gate**

### E2E (staging)

After Cloud Run deployment to staging:
1. Run `experiments/gato/experiments/exp-010/client/e2e_client.py` against the staging URL
2. Assert TTS audio received (duration > 5s)
3. Assert `gato_sessions_total{outcome="completed"}` increments in Prometheus
4. Assert process survives 5 minutes of idle (no memory growth, goroutines stable)
