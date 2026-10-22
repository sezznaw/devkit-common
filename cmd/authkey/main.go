// Command authkey prints a new Ed25519 key pair for the access tokens:
// AUTH_JWT_PRIVATE_KEY for the issuing service only (keep it in its Secret),
// AUTH_JWT_PUBLIC_KEY for the gateways (not secret).
//
//	go run github.com/sezznaw/devkit-common/cmd/authkey@latest
//
// authkey 生成访问令牌的 Ed25519 密钥对：私钥只给签发服务（放它自己的 Secret），公钥给网关（不保密）。
package main

import (
	"fmt"
	"os"

	"github.com/sezznaw/devkit-common/authx"
)

func main() {
	priv, pub, err := authx.GenerateKey()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("AUTH_JWT_PRIVATE_KEY=%s\nAUTH_JWT_PUBLIC_KEY=%s\n", priv, pub)
}
