package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"seo-monitor/internal/store"
)

const operationTimeout = 30 * time.Second

func main() {
	username := flag.String("username", "admin", "account whose password will be reset")
	allowWeakPassword := flag.Bool("allow-weak-password", false, "explicitly allow an 8-11 byte password (unsafe for public deployments)")
	flag.Parse()
	if flag.NArg() != 0 {
		fatalf("unexpected positional arguments")
	}

	password, err := readPassword(os.Stdin)
	if err != nil {
		fatalf("read password: %v", err)
	}

	mongoURI := strings.TrimSpace(os.Getenv("MONGODB_URI"))
	if mongoURI == "" {
		mongoURI = "mongodb://localhost:27017"
	}
	database := strings.TrimSpace(os.Getenv("MONGODB_DATABASE"))
	if database == "" {
		database = "seo_monitor"
	}

	ctx, cancel := context.WithTimeout(context.Background(), operationTimeout)
	defer cancel()

	databaseStore, err := store.New(ctx, mongoURI, database)
	if err != nil {
		fatalf("connect to database: %v", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		_ = databaseStore.Close(closeCtx)
	}()

	var resetErr error
	if *allowWeakPassword {
		fmt.Fprintln(os.Stderr, "WARNING: weak-password override enabled; public accounts remain vulnerable to guessing attacks.")
		resetErr = databaseStore.SetUserPasswordAllowWeak(ctx, *username, password)
	} else {
		resetErr = databaseStore.SetUserPassword(ctx, *username, password)
	}
	if resetErr != nil {
		if errors.Is(resetErr, store.ErrNotFound) {
			fatalf("user %q does not exist", *username)
		}
		fatalf("reset password: %v", resetErr)
	}
	fmt.Printf("Password updated for user %q; all existing sessions were revoked.\n", strings.ToLower(strings.TrimSpace(*username)))
}

func readPassword(input io.Reader) (string, error) {
	reader := bufio.NewReaderSize(input, 4096)
	password, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	password = strings.TrimSuffix(password, "\n")
	password = strings.TrimSuffix(password, "\r")
	return password, nil
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "error: "+format+"\n", args...)
	os.Exit(1)
}
