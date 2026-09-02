# DataGit — Makefile
#
# Phase 0 targets only. PLAN.md M0.2 specifies the full set
# (test-integration, test-crash, test-acceptance, verify-parity, proto);
# those land with the milestone that needs them.

PG17_DSN ?= postgres://datagit:datagit@localhost:55417/datagit
PG16_DSN ?= postgres://datagit:datagit@localhost:55416/datagit

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
test-integration: ## Integration tests against a real PostgreSQL (needs db-up)
	go test ./test/integration/ -v -count=1

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
db-up: ## Start PostgreSQL 16 and 17
	docker compose up -d
	@echo "pg17: $(PG17_DSN)"
	@echo "pg16: $(PG16_DSN)"

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

.PHONY: spike-s5
spike-s5: ## S5: storage growth and partition pruning
	go run ./spikes/s5_storage -mode sizes   -dsn "$(PG17_DSN)"
	go run ./spikes/s5_storage -mode pruning -dsn "$(PG17_DSN)"
