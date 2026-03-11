.PHONY: build sandbox test clean

# Build the forge binary
build:
	go build -o forge ./cmd/forge/

# Build the sandbox Docker image
# Gathers user dotfiles and Claude credentials if present, then builds.
sandbox:
	rm -rf .build-ctx
	mkdir -p .build-ctx/sandbox/dotfiles .build-ctx/sandbox/claude/settings
	cp Dockerfile.sandbox .build-ctx/Dockerfile
	cp sandbox/zshrc .build-ctx/sandbox/zshrc
	cp sandbox/tmux.conf .build-ctx/sandbox/tmux.conf
	# --- User dotfiles (optional) ---
	@if [ -f "$$HOME/.gitconfig" ]; then cp "$$HOME/.gitconfig" .build-ctx/sandbox/dotfiles/.gitconfig; fi
	@if [ -f "$$HOME/.vimrc" ]; then cp "$$HOME/.vimrc" .build-ctx/sandbox/dotfiles/.vimrc; fi
	@if [ -d "$$HOME/.config/starship.toml" ] || [ -f "$$HOME/.config/starship.toml" ]; then \
		mkdir -p .build-ctx/sandbox/dotfiles/.config && \
		cp "$$HOME/.config/starship.toml" .build-ctx/sandbox/dotfiles/.config/starship.toml; \
	fi
	# --- Claude credentials (optional) ---
	@if [ -f "$$HOME/.claude.json" ]; then cp "$$HOME/.claude.json" .build-ctx/sandbox/claude/.claude.json; fi
	@if [ -f "$$HOME/.claude/settings.json" ]; then \
		mkdir -p .build-ctx/sandbox/claude/settings && \
		cp "$$HOME/.claude/settings.json" .build-ctx/sandbox/claude/settings/settings.json; \
	fi
	@if command -v security >/dev/null 2>&1; then \
		security find-generic-password -s "Claude Code-credentials" -w > .build-ctx/sandbox/claude/settings/.credentials.json 2>/dev/null || true; \
	fi
	docker build -t forge-sandbox .build-ctx
	rm -rf .build-ctx

test:
	go test ./... -count=1 -timeout 60s

clean:
	rm -f forge
	rm -rf .build-ctx
