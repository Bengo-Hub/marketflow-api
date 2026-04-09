//go:build ignore
// +build ignore

package main

import (
	"log"

	"entgo.io/ent/entc"
	"entgo.io/ent/entc/gen"
)

func main() {
	err := entc.Generate("./schema", &gen.Config{
		Package: "github.com/bengobox/marketflow-api/internal/ent",
	})
	if err != nil {
		log.Fatalf("running ent codegen: %v", err)
	}
}
