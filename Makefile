# DataGit — Makefile
#
# Phase 0 targets only. PLAN.md M0.2 specifies the full set
# (test-integration, test-crash, test-acceptance, verify-parity, proto);
# those land with the milestone that needs them.

PG17_DSN ?= postgres://datagit:datagit@localhost:55417/datagit
PG16_DSN ?= postgres://datagit:datagit@localhost:55416/datagit
MYSQL_DSN ?= datagit:datagit@tcp(127.0.0.1:55484)/datagit?multiStatements=true

# The Python SDK's tooling. Point this at a virtualenv when the system Python is
# externally managed, which most are:
#   make sdk-py PYTHON=.venv/bin/python
PYTHON ?= python3

# Number of random operation sequences for the differential harness.
# 170000 x 60 ops is roughly 10M operations, the Phase 0 S2 bar.
SEQUENCES ?= 5000
OPS       ?= 60

.PHONY: help
help:
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) \
	  | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

# --- build and check ---------------------------------------------------------

.PHONY: build
build: ## Build everything
	go build ./...
	go build -o bin/datagit ./cmd/datagit
	go build -o bin/datagitd ./cmd/datagitd

.PHONY: lint
lint: ## go vet plus the internal/model import rule
	go vet ./...
	@$(MAKE) --no-print-directory check-model-imports

.PHONY: check-model-imports
check-model-imports: ## Enforce: internal/model is never imported by non-test code
	@# go list reports real dependencies. .Imports excludes test files, so a
	@# match here means production code depends on the reference model — which
	@# would mean the differential harness is comparing shared code with itself.
	@bad=$$(go list -f '{{$$p := .ImportPath}}{{range .Imports}}{{$$p}} -> {{.}}{{"\n"}}{{end}}' ./... \
	          | grep -- '-> github.com/Glyph-Software/datagit/internal/model$$' || true); \
	if [ -n "$$bad" ]; then \
	  echo "internal/model imported by non-test code:"; echo "$$bad"; exit 1; \
	fi; \
	echo "ok: internal/model is test-only"

.PHONY: test
test: ## Unit tests, race detector on
	go test -race ./internal/...

.PHONY: test-integration
test-integration: ## Integration tests on every supported engine (needs db-up)
	@$(MAKE) --no-print-directory test-integration-pg17
	@$(MAKE) --no-print-directory test-integration-pg16
	@$(MAKE) --no-print-directory test-integration-mysql

# The SAME suite runs against each engine. Nothing here is engine-specific, which
# is the point: a feature cannot ship working on one of them (§4.3, M5). An
# unreachable database named by DATAGIT_TEST_DSN FAILS rather than skipping, so a
# green run always means the suite actually ran.
.PHONY: test-integration-pg17
test-integration-pg17: ## Integration tests against PostgreSQL 17
	@echo "== PostgreSQL 17 =="
	DATAGIT_TEST_DSN="$(PG17_DSN)" go test ./test/integration/ -count=1

.PHONY: test-integration-pg16
test-integration-pg16: ## Integration tests against PostgreSQL 16
	@echo "== PostgreSQL 16 =="
	DATAGIT_TEST_DSN="$(PG16_DSN)" go test ./test/integration/ -count=1

.PHONY: test-integration-mysql
test-integration-mysql: ## Integration tests against MySQL 8.4
	@echo "== MySQL 8.4 =="
	DATAGIT_TEST_DSN="$(MYSQL_DSN)" go test ./test/integration/ -count=1

.PHONY: sdk-py
sdk-py: ## Regenerate the Python SDK stubs from the proto (PYTHON=... for a venv)
	$(PYTHON) -m grpc_tools.protoc -Iapi/proto \
	  --python_out=sdk/python --grpc_python_out=sdk/python --pyi_out=sdk/python \
	  api/proto/datagit/v1/datagit.proto

.PHONY: test-sdk-py
test-sdk-py: ## Python SDK tests (PYTHON=... for a venv)
	cd sdk/python && $(PYTHON) -m pytest tests/ -q

.PHONY: sdk-ts
sdk-ts: ## Regenerate the TypeScript SDK stubs from the proto
	npm install --no-audit --no-fund
	PATH="$(CURDIR)/node_modules/.bin:$$PATH" buf generate --template buf.gen.ts.yaml

.PHONY: test-sdk-ts
test-sdk-ts: ## TypeScript SDK tests
	npm run test:sdk

.PHONY: changeset
changeset: ## Record a version bump for the SDKs (both move together)
	npx changeset

.PHONY: sdk-version
sdk-version: ## Apply queued changesets locally (CI normally does this)
	@# The changelog links each entry to its pull request, which means a GitHub
	@# API lookup and therefore a token. CI passes one automatically; locally,
	@# export GITHUB_TOKEN (a `gh auth token` value works) before running this.
	npm run version-packages

.PHONY: check-sdk-versions
check-sdk-versions: ## Fail if the two SDK versions have drifted apart
	node scripts/sync-python-version.mjs --check

.PHONY: check-sdk-stubs
check-sdk-stubs: ## Fail if the committed stubs no longer match the proto
	@$(MAKE) --no-print-directory sdk-ts sdk-py
	@if ! git diff --exit-code -- sdk/typescript/src/gen sdk/python/datagit/v1; then \
	  echo; \
	  echo "The committed stubs do not match api/proto/datagit/v1/datagit.proto."; \
	  echo "Commit the regenerated files above."; \
	  exit 1; \
	fi; \
	echo "ok: the committed stubs match the proto"

.PHONY: test-bench
test-bench: ## Performance regression gates on every engine (M4.6, M5.3)
	@echo "== PostgreSQL 17 =="
	DATAGIT_TEST_DSN="$(PG17_DSN)"  go test ./test/bench/ -count=1 -v
	@echo "== PostgreSQL 16 =="
	DATAGIT_TEST_DSN="$(PG16_DSN)"  go test ./test/bench/ -count=1 -v
	@echo "== MySQL 8.4 =="
	DATAGIT_TEST_DSN="$(MYSQL_DSN)" go test ./test/bench/ -count=1 -v

.PHONY: test-acceptance
test-acceptance: ## Run the README tour verbatim against a real database (W5)
	go build -o bin/datagit ./cmd/datagit
	bash test/acceptance/tour.sh

.PHONY: test-frozen
test-frozen: ## Verify the frozen canonical encoding and commit hash (M0.4, W3)
	go test ./internal/hash/ -v

.PHONY: proto
proto: ## Generate Go from the protobuf definitions
	buf lint
	buf generate

# --- the primary correctness evidence (PLAN.md W1) ---------------------------

.PHONY: test-property
test-property: ## Differential harness: reference model vs engine
	go test ./test/property/ -run TestDifferential \
	  -sequences $(SEQUENCES) -ops $(OPS) -timeout 180m

.PHONY: test-property-full
test-property-full: ## Full ~10M-operation sweep
	$(MAKE) test-property SEQUENCES=170000 OPS=60

.PHONY: test-regressions
test-regressions: ## Replay the seed corpus and the minimized repros
	go test ./test/property/ -run 'TestSeedCorpus|TestReproShrunk' -v

# --- development databases ---------------------------------------------------

.PHONY: db-up
db-up: ## Start PostgreSQL 16 and 17 and MySQL 8.4
	docker compose up -d
	@echo "pg17:  $(PG17_DSN)"
	@echo "pg16:  $(PG16_DSN)"
	@echo "mysql: $(MYSQL_DSN)"

.PHONY: db-down
db-down: ## Stop the databases (keeps volumes)
	docker compose down

.PHONY: db-reset
db-reset: ## Stop and DELETE all database volumes
	docker compose down -v

# --- Phase 0 spikes (throwaway; see docs/phase0/findings.md) -----------------

.PHONY: spike-s1
spike-s1: ## S1: generate the 50M-version dataset, then check correctness and benchmark
	bash spikes/s1_resolution/generate.sh
	go run ./spikes/s1_resolution -mode correctness -dsn "$(PG17_DSN)"
	go run ./spikes/s1_resolution -mode bench       -dsn "$(PG17_DSN)"

.PHONY: spike-s1-correctness
spike-s1-correctness: ## S1: the two §7.3 resolution hazards only
	go run ./spikes/s1_resolution -mode correctness -dsn "$(PG17_DSN)"

.PHONY: spike-s1-explain
spike-s1-explain: ## S1: EXPLAIN ANALYZE for each resolution shape
	go run ./spikes/s1_resolution -mode explain -dsn "$(PG17_DSN)"

.PHONY: spike-s3
spike-s3: ## S3: commit latency, concurrency, and write amplification
	go run ./spikes/s3_commit -mode setup         -dsn "$(PG17_DSN)"
	go run ./spikes/s3_commit -mode latency       -dsn "$(PG17_DSN)"
	go run ./spikes/s3_commit -mode concurrency   -dsn "$(PG17_DSN)"
	go run ./spikes/s3_commit -mode amplification -dsn "$(PG17_DSN)"

.PHONY: spike-s4
spike-s4: ## S4: crash-resume convergence for the migration state machine
	go run ./spikes/s4_migration -engine mysql
	go run ./spikes/s4_migration -engine postgres

.PHONY: spike-s5
spike-s5: ## S5: storage growth and partition pruning
	go run ./spikes/s5_storage -mode sizes   -dsn "$(PG17_DSN)"
	go run ./spikes/s5_storage -mode pruning -dsn "$(PG17_DSN)"
