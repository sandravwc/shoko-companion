TARGETS = linux-amd64 linux-arm64 windows-amd64

all: $(TARGETS)

$(TARGETS):
	CGO_ENABLED=0 GOOS=$(word 1,$(subst -, ,$@)) GOARCH=$(word 2,$(subst -, ,$@)) \
	go build -trimpath -ldflags='-s -w' -o dist/shokod-$@$(if $(findstring windows,$@),.exe) ./cmd/shokod

test:
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
