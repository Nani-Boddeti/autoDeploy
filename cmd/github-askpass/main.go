// github-askpass is intentionally tiny: Git invokes it for credential prompts
// and it reads a single password from an inherited anonymous pipe.
package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		os.Exit(2)
	}
	prompt := os.Args[1]
	if strings.Contains(strings.ToLower(prompt), "username") {
		_, _ = fmt.Fprintln(os.Stdout, "x-access-token")
		return
	}
	if !strings.Contains(strings.ToLower(prompt), "password") {
		os.Exit(2)
	}
	fd, err := strconv.Atoi(os.Getenv("AUTODEPLOY_GIT_ASKPASS_FD"))
	if err != nil || fd < 3 {
		os.Exit(2)
	}
	f := os.NewFile(uintptr(fd), "checkout-token")
	if f == nil {
		os.Exit(2)
	}
	b, err := io.ReadAll(io.LimitReader(f, 8193))
	_ = f.Close()
	if err != nil || len(b) == 0 || len(b) > 8192 || b[len(b)-1] != '\n' || strings.ContainsAny(string(b[:len(b)-1]), "\r\n\x00") {
		os.Exit(2)
	}
	_, _ = os.Stdout.Write(b)
}
