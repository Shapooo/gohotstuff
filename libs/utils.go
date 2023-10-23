package libs

import (
	"crypto/sha256"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"
)

var envCfg string

func GenRandomID() uint64 {
	nano := time.Now().UnixNano()
	r := rand.New(rand.NewSource(nano))
	randNum1 := r.Int63()
	randNum2 := r.Int63()
	shift1 := r.Intn(16) + 2
	shift2 := r.Intn(8) + 1

	randId := ((randNum1 >> uint(shift1)) + (randNum2 >> uint(shift2)) + (nano >> 1)) &
		0x7FFFFFFFFFFFFFFF
	return uint64(randId)

}

func GetSum(b []byte) string {
	h := sha256.New()
	h.Write(b)
	bs := h.Sum(nil)
	return fmt.Sprintf("%x", bs)
}

func F(input []byte) string {
	return string(input)
}

func SetRootDir(path string) {
	if len(envCfg) <= 0 {
		envCfg = path
	}
}

func GetCurExecDir() string {
	curDir, _ := filepath.Abs(filepath.Dir(os.Args[0]))
	return curDir
}

func GetCurRootDir() string {
	if len(envCfg) <= 0 {
		curExecDir := GetCurExecDir()
		envCfg = curExecDir
	}
	return envCfg
}

func FileIsExist(name string) bool {
	if _, err := os.Stat(name); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}

	return true
}

func MakeDir(name string) error {
	err := os.MkdirAll(name, 0755)
	if err != nil {
		return err
	}
	return nil
}
