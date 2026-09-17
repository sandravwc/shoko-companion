TARGETS = linux-amd64 linux-arm64 windows-amd64

all: $(TARGETS)

$(TARGETS):
	CGO_ENABLED=0 GOOS=$(word 1,$(subst -, ,$@)) GOARCH=$(word 2,$(subst -, ,$@)) \
	go build -trimpath -ldflags='-s -w' -o dist/shokod-$@$(if $(findstring windows,$@),.exe) ./cmd/shokod

test: lint-readme
	go vet ./... && go test ./...

# user-level install: binary + systemd --user unit reading ~/.config/shokod.env
install: linux-amd64
	install -Dm755 dist/shokod-linux-amd64 ~/.local/bin/shokod
	install -Dm644 shokod.service ~/.config/systemd/user/shokod.service
	systemctl --user daemon-reload
	systemctl --user enable --now shokod
	systemctl --user restart shokod

clean:
	rm -rf dist

.PHONY: all test install clean $(TARGETS)

# diagram in README: every line same width, box walls in the same column
lint-readme:
	@python3 -c "import sys;s=open('README.md').read();a=s.index('\`\`\`\n',s.index('## Goal'))+4;L=s[a:s.index('\`\`\`',a)].rstrip('\n').split('\n');W={len(l) for l in L};C={l.find('│') for l in L if '│' in l};sys.exit(0 if len(W)==1 and len(C)==1 else 'diagram not aligned: widths %s wall-cols %s'%(W,C))"
