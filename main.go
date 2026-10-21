// Command eh-blacklist-server starts the EH blacklist API.
//
// This entry point exists so that both `go run .` and `go run ./cmd/server`
// work. The actual implementation lives in internal/blacklist.
package main

import "eh-blacklist-server/internal/blacklist"

func main() {
	blacklist.Main()
}
