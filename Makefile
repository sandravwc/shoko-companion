TARGETS = linux-amd64 linux-arm64 windows-amd64

all: $(TARGETS)

$(TARGETS):
	CGO_ENABLED=0 GOOS=$(word 1,$(subst -, ,$@)) GOARCH=$(word 2,$(subst -, ,$@)) \
	go build -trimpath -ldflags='-s -w' -o dist/shokod-$@$(if $(findstring windows,$@),.exe) ./cmd/shokod

clean:
	rm -rf dist

.PHONY: all clean $(TARGETS)
