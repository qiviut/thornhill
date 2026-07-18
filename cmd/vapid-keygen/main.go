// Command vapid-keygen emits a Web Push VAPID key pair as dotenv entries.
// Redirect its output to a mode-0600 deployment environment file; the private
// key must never be committed or logged.
package main

import (
	"flag"
	"fmt"
	"net/url"
	"os"
	"strings"

	webpush "github.com/SherClockHolmes/webpush-go"
)

func main() {
	subject := flag.String("subject", "", "operator contact URI (mailto: or https:)")
	flag.Parse()
	u, err := url.Parse(strings.TrimSpace(*subject))
	if err != nil || (u.Scheme != "mailto" && u.Scheme != "https") ||
		(u.Scheme == "https" && u.Host == "") ||
		(u.Scheme == "mailto" && strings.TrimSpace(u.Opaque+u.Path) == "") {
		fmt.Fprintln(os.Stderr, "-subject must be an absolute mailto: or https: operator contact URI")
		os.Exit(2)
	}
	privateKey, publicKey, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate VAPID keys:", err)
		os.Exit(1)
	}
	fmt.Printf("PUSH_VAPID_PUBLIC_KEY=%s\nPUSH_VAPID_PRIVATE_KEY=%s\nPUSH_VAPID_SUBJECT=%s\n", publicKey, privateKey, *subject)
}
