package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/backpack/backpack/internal/node"
)

// node exec is the stdin/stdout RPC used by paired CLI setup over SSH.
const nodeUsage = `backpack node - execute a management request over SSH

  backpack node exec -
        Read one base64-encoded JSON request from stdin and print its response.
        Intended for CLI automation and paired server setup.
`

func runNode(args []string) {
	if len(args) == 0 {
		fmt.Print(nodeUsage)
		os.Exit(2)
	}
	switch args[0] {
	case "exec":
		nodeExec(args[1:])
	case "-h", "--help", "help":
		fmt.Print(nodeUsage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", args[0], nodeUsage)
		os.Exit(2)
	}
}

// nodeExec performs one operation for a panel reaching this server over SSH.
//
// The request arrives on stdin when the argument is "-". It used to arrive as
// the argument itself, and a request that sets up a tunnel carries the
// tunnel's token — which, as an argument, any unprivileged user on this machine
// could read from the process list for as long as the command ran. The
// argument form is still read, for a panel older than this build. Base64
// either way: nothing in it can be read as shell syntax, whatever it holds.
//
// The answer always goes to stdout, including a refusal — the panel reads a
// Response either way, and a command that failed with nothing on stdout would
// reach it as "the far end said nothing", which is what a broken SSH looks like
// and is a different problem. The exit status stays 0 for the same reason; a
// non-zero one means this command failed, not that the operation did.
func nodeExec(args []string) {
	if len(args) != 1 {
		fmt.Fprintln(os.Stderr, "node exec takes one base64 request, or - to read it from stdin")
		os.Exit(2)
	}
	encoded := args[0]
	if encoded == "-" {
		in, err := io.ReadAll(io.LimitReader(os.Stdin, 16<<20))
		if err != nil {
			fmt.Fprintln(os.Stderr, "could not read the request:", err)
			os.Exit(2)
		}
		encoded = strings.TrimSpace(string(in))
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		fmt.Fprintln(os.Stderr, "the request is not valid base64:", err)
		os.Exit(2)
	}
	var req node.Request
	if err := json.Unmarshal(raw, &req); err != nil {
		fmt.Fprintln(os.Stderr, "the request is not valid JSON:", err)
		os.Exit(2)
	}
	out, err := json.Marshal(node.Execute(req))
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not encode the answer:", err)
		os.Exit(1)
	}
	fmt.Println(base64.StdEncoding.EncodeToString(out))
}
