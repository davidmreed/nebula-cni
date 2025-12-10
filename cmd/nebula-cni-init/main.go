package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/slackhq/nebula-cni/internal"
	"github.com/slackhq/nebula/cert"
)

// A version string that can be set with
//
//	-ldflags "-X main.Build=SOMEVERSION"
//
// at compile-time.
var Build string

func main() {
	if Build == "" {
		Build = internal.ModuleVersion()
	}
	os.Exit(_main())
}

func _main() int {
	server := &http.Server{
		Addr: net.JoinHostPort("", internal.InitPort),
	}

	podIP := os.Getenv("POD_IP")
	if podIP == "" {
		slog.Error("POD_IP is missing")
		return 1
	}

	nebulaIP := internal.CalculateNebulaIP(podIP)

	existing, err := checkExistingCNI(nebulaIP)
	if err != nil {
		slog.Error("checkExistingCNI returned an error, continuing", "error", err)
	}
	if existing {
		slog.Info("CNI already exists, exiting early")
		return 0
	}

	http.HandleFunc("HEAD /log", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(internal.HeaderVersion, Build)

		// indicate we support /log
		w.WriteHeader(http.StatusNoContent)
	})

	http.HandleFunc("POST /log", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(internal.HeaderVersion, Build)

		s := bufio.NewScanner(r.Body)
		for s.Scan() {
			fmt.Fprintln(os.Stderr, "nebula-cni-instance:", s.Text())
		}
		if err := s.Err(); err != nil {
			slog.Error("scanner.Scan", "error", err)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	http.HandleFunc("GET /token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(internal.HeaderVersion, Build)

		token, err := os.ReadFile("/var/run/secrets/nebula-cni/serviceaccount/token")
		if err != nil {
			slog.Error("failed to read token", "error", err)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		parts := bytes.SplitN(token, []byte("."), 3)
		slog.Info("token file read", "header", string(parts[0]), "payload", string(parts[1]))

		w.Header().Set("Content-Type", "text/plain")
		_, err = w.Write(token)
		if err != nil {
			slog.Error("failed to write /token response", "error", err)
		}
	})

	http.HandleFunc("POST /init", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(internal.HeaderVersion, Build)

		decoder := json.NewDecoder(r.Body)

		var body internal.InitPostBody
		err := decoder.Decode(&body)
		if err != nil {
			slog.Error("decoder.Decode", "error", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		// Discard anything remaining after the first JSON object
		_, err = io.Copy(io.Discard, r.Body)
		if err != nil && !errors.Is(err, io.EOF) {
			slog.Error("io.Discard", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		remoteHost, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			slog.Error("remoteHost invalid", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if remoteHost != nebulaIP {
			slog.Error("remoteHost is not POD_IP", "remote_host", remoteHost, "pod_ip", nebulaIP)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if body.NebulaCertificate == "" {
			slog.Error("nebula_certificate is required")
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		cert, _, err := cert.UnmarshalCertificateFromPEM([]byte(body.NebulaCertificate))
		if err != nil {
			slog.Error("invalid nebula_certificate", "error", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		err = os.WriteFile("/nebula/host.crt", []byte(body.NebulaCertificate), 0644)
		if err != nil {
			slog.Error("Failed to write /nebula/host.crt", "error", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		slog.Info("wrote certificate")
		fmt.Fprintln(os.Stderr, cert.String())

		w.WriteHeader(http.StatusNoContent)

		slog.Info("responded to CNI request")

		go func() {
			slog.Info("triggering shutdown")
			if err := server.Shutdown(context.Background()); err != nil {
				slog.Error("server.Close", "error", err)
			}
		}()
	})

	slog.Info("listening and waiting for CNI", "addr", server.Addr, "init-version", Build)
	if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		slog.Error("HTTP server error", "error", err)
		return 1
	}

	slog.Info("finished")
	return 0
}

// TODO Kube sometimes reruns the init containers, does it also re-run the CNI scripts?
// - https://kubernetes.io/docs/concepts/workloads/pods/init-containers/#pod-restart-reasons
// Can we just check if we already have a host.crt to detect this case?
func checkExistingCNI(nebulaIP string) (bool, error) {
	hostCertPEM, err := os.ReadFile("/nebula/host.crt")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Expected
			return false, nil
		}
		return false, err
	}

	// The file exists..., check if the existing nebula cert matches our IP
	// If it doesn't, the CNI is probably also restarting

	cert, _, err := cert.UnmarshalCertificateFromPEM([]byte(hostCertPEM))
	if err != nil {
		return false, err
	}

	existingIP := cert.Networks()[0].Addr().String()
	if existingIP != nebulaIP {
		return false, fmt.Errorf("existing certificate doesn't match POD_IP: %s != %s", existingIP, nebulaIP)
	}
	if cert.Expired(time.Now()) {
		return false, fmt.Errorf("existing cert is expired: %v", cert.String())
	}

	if _, err := net.InterfaceByName("tun1"); err != nil {
		return false, errors.New("host.crt found but no tun1")
	}

	return true, nil
}
