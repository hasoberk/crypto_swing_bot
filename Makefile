BINARY := swingbot
CMD    := ./cmd/swingbot
BIN    := bin/$(BINARY)

.PHONY: build test vet lint run clean

build: ## Derle: bin/swingbot üretir.
	go build -o $(BIN) $(CMD)

test: ## Tüm paketlerin testlerini çalıştır.
	go test ./...

vet: ## go vet ile statik kontrol.
	go vet ./...

lint: vet ## golangci-lint varsa onunla, yoksa go vet ile denetim yap.
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "golangci-lint bulunamadı, go vet ile devam edildi (yukarıda)."; \
	fi

run: build ## Derle ve çalıştır. Örn: make run ARGS="data backfill --years=3"
	$(BIN) $(ARGS)

clean: ## Derleme çıktısını sil.
	rm -rf bin
