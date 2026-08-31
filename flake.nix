{
  description = "Go development environment using nixpkgs-unstable";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        
        # Your custom build command cleanly defined here
        build-script = pkgs.writeShellScriptBin "build-linko" ''
          SHA=$(git rev-parse HEAD 2>/dev/null || echo "unknown")
          TIME=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
          # Using the full boot.dev paths you specified
          go build -ldflags "-X boot.dev/linko/internal/build.GitSHA=$SHA -X boot.dev/linko/internal/build.BuildTime=$TIME" "$@"
        '';
      in
      {
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            build-script
            go
            gopls # language server
            gotools # goimports, etc.
            go-tools # staticcheck
            delve # debugger
            bootdev-cli
            docker
          ];

          shellHook = ''
            echo "Using $(go version)"
            echo "Tip: Use 'build-linko' instead of 'go build' to compile with version flags!"
            
            export GOPATH="$HOME/go"
            export PATH="$GOPATH/bin:$PATH"
          '';
        };
      }
    );
}
