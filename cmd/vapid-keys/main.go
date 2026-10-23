// Command vapid-keys prints a new VAPID key pair for Web Push, ready to paste
// into the environment. Generate one pair per instance and keep it: browsers
// subscribed with the public key stop receiving pushes when it changes.
package main

import (
	"fmt"
	"os"

	webpush "github.com/SherClockHolmes/webpush-go"
)

func main() {
	privateKey, publicKey, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		fmt.Fprintln(os.Stderr, "generate VAPID keys:", err)
		os.Exit(1)
	}
	fmt.Printf("ORBIT_VAPID_PUBLIC_KEY=%s\nORBIT_VAPID_PRIVATE_KEY=%s\n", publicKey, privateKey)
}
