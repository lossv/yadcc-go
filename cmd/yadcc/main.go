package main

import (
	"os"

	"yadcc-go/internal/client"
)

func main() {
	os.Exit(client.Run(os.Args))
}
