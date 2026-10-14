// Command conch-adapter is Conch's reference runtime adapter: it answers a
// channel by running a command and tells the server it is there. The program
// is in internal/adapter.
package main

import "github.com/njdaniel/conch/internal/adapter"

func main() { adapter.Main() }
