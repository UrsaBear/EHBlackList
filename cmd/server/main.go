// Command server starts the EH blacklist API.
//
// It is kept for compatibility with the original project layout; `go run .`
// from the repository root behaves identically.
package main

import "eh-blacklist-server/internal/blacklist"

func main() {
	blacklist.Main()
}
