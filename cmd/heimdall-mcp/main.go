// heimdall-mcp is a stdio MCP server that provides local semantic code
// search powered by Ollama embeddings (BGE-M3) and a SQLite vector store.
//
// It enables any MCP client (Claude Code, Claude Desktop, etc.) to search,
// index, and inspect locally indexed project files — all running on your
// machine with no cloud APIs.
//
// Usage:
//
//	claude mcp add heimdall /path/to/heimdall-mcp
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"os"

	"github.com/caio-silva/heimdall-mcp/internal/cli"
	"github.com/caio-silva/heimdall-mcp/internal/config"
	"github.com/caio-silva/heimdall-mcp/internal/heimdall"
	"github.com/caio-silva/heimdall-mcp/internal/mcp"
	"github.com/caio-silva/heimdall-mcp/internal/registry"
)

func main() {
	log.SetOutput(os.Stderr)
	log.SetPrefix("heimdall-mcp: ")

	// Migrate legacy .viking_db directory to .heimdall_db in CWD
	cwd, _ := os.Getwd()
	if cwd != "" {
		heimdall.MigrateDBDir(cwd)
	}

	cfg := config.LoadConfig()

	// CLI mode: if args are provided, run as a command-line tool
	if len(os.Args) > 1 {
		cli.RunCLI(cfg, os.Args[1:])
		return
	}

	// Open global memory store
	memDBPath := config.ResolveMemoryDBPath()
	memStore, err := heimdall.OpenMemoryStore(memDBPath)
	if err != nil {
		log.Fatalf("failed to open memory store at %s: %v", memDBPath, err)
	}
	defer memStore.Close()

	// MCP mode: no args, run as stdio MCP server
	s := &mcp.Server{Cfg: cfg, Registry: registry.LoadRegistry(), MemoryStore: memStore}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req mcp.JSONRPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			log.Printf("parse error: %v", err)
			continue
		}

		resp := s.Handle(req)
		if resp == nil {
			continue
		}

		out, _ := json.Marshal(resp)
		fmt.Fprintf(os.Stdout, "%s\n", out)
	}
}
