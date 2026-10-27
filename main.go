package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	appVersion            = "1.0.0"
	defaultMaxNoteBytes   = 1 << 20
	maxAllowedNoteBytes   = 16 << 20
	minAllowedNoteBytes   = 1 << 10
	defaultListenAddress  = ":8080"
	defaultDataDirectory  = "data"
	noteIDAlphabet        = "234579abcdefghjkmnpqrstwxyz"
	generatedNoteIDLength = 8
)

func main() {
	addr := flag.String("addr", envString("NOTEBOOK_ADDR", defaultListenAddress), "HTTP listen address")
	dataDir := flag.String("data", envString("NOTEBOOK_DATA_DIR", defaultDataDirectory), "directory for note files")
	maxBytes := flag.Int64("max-bytes", envInt64("NOTEBOOK_MAX_BYTES", defaultMaxNoteBytes), "maximum note size in bytes")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println("notebook", appVersion)
		return
	}
	if *maxBytes < minAllowedNoteBytes || *maxBytes > maxAllowedNoteBytes {
		log.Fatalf("NOTEBOOK_MAX_BYTES/-max-bytes must be between %d and %d bytes", minAllowedNoteBytes, maxAllowedNoteBytes)
	}
	if strings.TrimSpace(*addr) == "" {
		log.Fatal("listen address must not be empty")
	}
	if strings.TrimSpace(*dataDir) == "" {
		log.Fatal("data directory must not be empty")
	}

	store, err := newNoteStore(*dataDir, *maxBytes)
	if err != nil {
		log.Fatalf("initialise data directory %q: %v", *dataDir, err)
	}
	handler, err := newApp(store)
	if err != nil {
		log.Fatalf("initialise application: %v", err)
	}

	server := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       20 * time.Second,
		WriteTimeout:      20 * time.Second,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		log.Printf("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	log.Printf("notebook %s listening on %s (data: %s, max note: %s)", appVersion, *addr, store.dir, humanBytes(store.maxBytes))
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
	<-shutdownDone
}

func envString(name, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		return value
	}
	return fallback
}

func envInt64(name string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		log.Fatalf("invalid %s %q: must be an integer number of bytes", name, value)
	}
	return parsed
}

func randomNoteID(length int) (string, error) {
	if length <= 0 || length > 256 {
		return "", fmt.Errorf("invalid note id length %d", length)
	}

	const alphabetLength = len(noteIDAlphabet)
	const unbiasedLimit = 256 - (256 % alphabetLength)
	result := make([]byte, length)
	randomBytes := make([]byte, length*2)
	position := 0

	for position < length {
		if _, err := rand.Read(randomBytes); err != nil {
			return "", err
		}
		for _, b := range randomBytes {
			if int(b) >= unbiasedLimit {
				continue
			}
			result[position] = noteIDAlphabet[int(b)%alphabetLength]
			position++
			if position == length {
				break
			}
		}
	}
	return string(result), nil
}
