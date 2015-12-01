package main

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"syscall"
)

const (
	// max open file should at least be
	_MaxOpenfile              uint64 = 1024 * 1024 * 1024
	_MaxBackendAddrCacheCount int    = 1024 * 1024
	_DefaultPort                     = "4043"
)

var (
	_SecretPassphase []byte
	_Salt            []byte
)

var (
	_BackendAddrCacheMutex = new(sync.Mutex)
	_BackendAddrCache      atomic.Value
)

func init() {
	_BackendAddrCache.Store(make(map[string]string))
}

func readBackendAddrCache(key string) (string, bool) {
	m1 := _BackendAddrCache.Load().(map[string]string)

	val, ok := m1[key]
	return val, ok
}

func writeBackendAddrCache(key, val string) {
	_BackendAddrCacheMutex.Lock()
	defer _BackendAddrCacheMutex.Unlock()

	m1 := _BackendAddrCache.Load().(map[string]string)
	m2 := make(map[string]string) // create a new value

	// flush cache if there is way too many
	if len(m1) < _MaxBackendAddrCacheCount {
		// copy-on-write
		for k, v := range m1 {
			m2[k] = v // copy all data from the current object to the new one
		}
	}

	m2[key] = val
	_BackendAddrCache.Store(m2) // atomically replace the current object with the new one
}

func pipe(dst io.Writer, src io.Reader, wg *sync.WaitGroup) {
	defer func() {
		wg.Done()
		if r := recover(); r != nil {
			log.Println("Recovered in", r, ":", string(debug.Stack()))
		}
	}()
	wg.Add(1)
	_, err := io.Copy(dst, src)
	// handle error
	log.Println(err)
}

// TCPServer is handler for all tcp queries
func TCPServer(l net.Listener) {
	defer l.Close()
	for {
		// Wait for a connection.
		conn, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}
		// Handle the connection in a new goroutine.
		// The loop then returns to accepting, so that
		// multiple connections may be served concurrently.
		go func(c net.Conn) {
			defer func() {
				if r := recover(); r != nil {
					log.Println("Recovered in", r, ":", string(debug.Stack()))
				}
			}()
			defer c.Close()

			// TODO: binary mode if first byte is 0x00

			rdr := bufio.NewReader(c)
			// Read first line
			line, isPrefix, err := rdr.ReadLine()
			if err != nil || isPrefix {
				// handle error
				log.Panicln(err)
			}

			// Try to check cache
			addr, ok := readBackendAddrCache(string(line))
			if !ok {
				// Try to decode it (base64)
				data, err := base64.StdEncoding.DecodeString(string(line))
				if err != nil {
					log.Panicln("error:", err)
					return
				}

				// Try to decrypt it (AES)
				block, err := aes.NewCipher(_SecretPassphase)
				if err != nil {
					log.Panicln("error:", err)
				}
				if len(data) < aes.BlockSize {
					log.Panicln("error:", errors.New("ciphertext too short"))
				}
				iv := data[:aes.BlockSize]
				text := data[aes.BlockSize:]
				cfb := cipher.NewCFBDecrypter(block, iv)
				cfb.XORKeyStream(text, text)

				// Check and remove the salt
				if len(text) < len(_Salt) {
					log.Panicln("error:", errors.New("salt check failed"))
				}

				addrLength := len(text) - len(_Salt)
				if !bytes.Equal(text[addrLength:], _Salt) {
					log.Panicln("error:", errors.New("salt not match"))
				}

				addr = string(text[:addrLength])

				// Write to cache
				writeBackendAddrCache(string(line), addr)
			}

			// Build tunnel
			backend, err := net.Dial("tcp", addr)
			if err != nil {
				// handle error
				log.Panicln(err)
			}
			defer backend.Close()

			// Start transfering data
			var wg sync.WaitGroup

			go pipe(c, backend, &wg)
			go pipe(backend, c, &wg)

			wg.Wait()
			// handle error
			log.Panicln(err)

		}(conn)
	}
}

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	os.Setenv("GOTRACEBACK", "crash")

	lim := syscall.Rlimit{}
	syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim)
	if lim.Cur < _MaxOpenfile || lim.Max < _MaxOpenfile {
		lim.Cur = _MaxOpenfile
		lim.Max = _MaxOpenfile
		syscall.Setrlimit(syscall.RLIMIT_NOFILE, &lim)
	}

	_Salt = []byte(os.Getenv("SALT"))
	_SecretPassphase = []byte(os.Getenv("SECRET"))

	ln, err := net.Listen("tcp", ":"+_DefaultPort)
	if err != nil {
		log.Fatal(err)
	}

	TCPServer(ln)
}
