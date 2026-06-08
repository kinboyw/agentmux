package control

import (
	"io"
	"math/rand"
	"os"
)

func DefaultIO() (io.Reader, io.Writer) {
	return os.Stdin, os.Stdout
}

func randomInt63() int64 {
	return rand.Int63()
}
