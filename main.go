package main

import (
	_ "embed"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/filecoin-saturn/caboose"
	"github.com/ipfs/go-blockservice"
	offline "github.com/ipfs/go-ipfs-exchange-offline"
	"github.com/ipfs/go-libipfs/gateway"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/spf13/cobra"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	"github.com/ipfs/go-blockservice/tikv"
)

func main() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func init() {
	rootCmd.Flags().String("uploader-endpoint", "", "url of the uploader endpoint")
	rootCmd.Flags().StringSlice("kubo-rpc", []string{}, "Kubo RPC nodes that will handle /api/v0 requests (can be set multiple times)")
	rootCmd.Flags().Int("gateway-port", 8080, "gateway port")
	rootCmd.Flags().Int("metrics-port", 8040, "metrics port")

	rootCmd.MarkFlagRequired("uploader-endpoint")
	rootCmd.MarkFlagRequired("kubo-rpc")
}

var rootCmd = &cobra.Command{
	Use:               "ipfs-gateway",
	Version:           version,
	CompletionOptions: cobra.CompletionOptions{DisableDefaultCmd: true},
	Short:             "IPFS Gateway implementation for https://github.com/protocol/bifrost-infra",
	RunE: func(cmd *cobra.Command, args []string) error {
		uploaderEndpoint, _ := cmd.Flags().GetString("uploader-endpoint")
		kuboRPC, _ := cmd.Flags().GetStringSlice("kubo-rpc")
		gatewayPort, _ := cmd.Flags().GetInt("gateway-port")
		metricsPort, _ := cmd.Flags().GetInt("metrics-port")

		log.Printf("Starting ipfs-gateway %s", version)

		tikv.InitStore()
		gatewaySrv, err := makeGatewayHandler(uploaderEndpoint, kuboRPC, gatewayPort)
		if err != nil {
			return err
		}

		metricsSrv, err := makeMetricsHandler(metricsPort)
		if err != nil {
			return err
		}

		quit := make(chan os.Signal, 1)
		var wg sync.WaitGroup
		wg.Add(2)

		go func() {
			defer wg.Done()

			log.Printf("Path gateway listening on http://127.0.0.1:%d", gatewayPort)
			log.Printf("  Smoke test (JPG): http://127.0.0.1:%d/ipfs/bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi", gatewayPort)
			log.Printf("Subdomain gateway configured on dweb.link and http://localhost:%d", gatewayPort)
			log.Printf("  Smoke test (Subdomain+DNSLink+UnixFS+HAMT): http://localhost:%d/ipns/en.wikipedia-on-ipfs.org/wiki/", gatewayPort)
			log.Printf("Legacy RPC at /api/v0 provided by %s", strings.Join(kuboRPC, " "))
			err := gatewaySrv.ListenAndServe()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("Failed to start gateway: %s", err)
				quit <- os.Interrupt
			}
		}()

		go func() {
			defer wg.Done()
			log.Printf("Metrics exposed at http://127.0.0.1:%d/debug/metrics/prometheus", metricsPort)
			err := metricsSrv.ListenAndServe()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("Failed to start metrics: %s", err)
				quit <- os.Interrupt
			}
		}()

		signal.Notify(quit, os.Interrupt)
		<-quit
		log.Printf("Closing servers...")
		go gatewaySrv.Close()
		go metricsSrv.Close()
		wg.Wait()
		return nil
	},
}

func makeGatewayHandler(uploaderEndpoint string ,kuboRPC []string, port int) (*http.Server, error) {
	var bs blockstore.Blockstore

	// Sets up an offline (no exchange) blockService based on the Saturn block store.
	blockService := blockservice.New(bs, offline.Exchange(bs))

	blockservice.InitUploader(uploaderEndpoint)

	// // Sets up the routing system, which will proxy the IPNS routing requests to the given gateway.
	routing := newProxyRouting(kuboRPC)

	// Creates the gateway with the block service and the routing.
	gwAPI, err := newBifrostGateway(blockService, routing)
	if err != nil {
		return nil, err
	}

	headers := map[string][]string{}
	gateway.AddAccessControlHeaders(headers)

	gwConf := gateway.Config{
		Headers: headers,
	}

	gwHandler := gateway.NewHandler(gwConf, gwAPI)
	mux := http.NewServeMux()
	mux.Handle("/ipfs/", gwHandler)
	mux.Handle("/ipns/", gwHandler)
	mux.Handle("/api/v0/", newAPIHandler(kuboRPC))

	// Note: in the future we may want to make this more configurable.
	noDNSLink := false
	publicGateways := map[string]*gateway.Specification{
		"localhost": {
			Paths:         []string{"/ipfs", "/ipns"},
			NoDNSLink:     noDNSLink,
			UseSubdomains: true,
		},
		"dweb.link": {
			Paths:         []string{"/ipfs", "/ipns"},
			NoDNSLink:     noDNSLink,
			UseSubdomains: true,
		},
	}

	// Creates metrics handler for total response size. Matches the same metrics
	// from Kubo:
	// https://github.com/ipfs/kubo/blob/e550d9e4761ea394357c413c02ade142c0dea88c/core/corehttp/metrics.go#L79-L152
	sum := prometheus.NewSummaryVec(prometheus.SummaryOpts{
		Namespace:  "ipfs",
		Subsystem:  "http",
		Name:       "response_size_bytes",
		Help:       "The HTTP response sizes in bytes.",
		Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
	}, nil)
	err = prometheus.Register(sum)
	if err != nil {
		return nil, err
	}

	// Construct the HTTP handler for the gateway.
	handler := http.Handler(gateway.WithHostname(mux, gwAPI, publicGateways, noDNSLink))
	handler = promhttp.InstrumentHandlerResponseSize(sum, handler)

	return &http.Server{
		Handler: handler,
		Addr:    ":" + strconv.Itoa(port),
	}, nil
}

func makeMetricsHandler(port int) (*http.Server, error) {
	mux := http.NewServeMux()

	gatherers := prometheus.Gatherers{
		prometheus.DefaultGatherer,
		caboose.CabooseMetrics,
	}
	options := promhttp.HandlerOpts{}
	mux.Handle("/debug/metrics/prometheus", promhttp.HandlerFor(gatherers, options))

	return &http.Server{
		Handler: mux,
		Addr:    ":" + strconv.Itoa(port),
	}, nil
}

func newAPIHandler(endpoints []string) http.Handler {
	mux := http.NewServeMux()

	// Endpoints that can be redirected to the gateway itself as they can be handled
	// by the path gateway.
	mux.HandleFunc("/api/v0/cat", func(w http.ResponseWriter, r *http.Request) {
		cid := r.URL.Query().Get("arg")
		url := fmt.Sprintf("/ipfs/%s", cid)
		http.Redirect(w, r, url, http.StatusFound)
	})

	mux.HandleFunc("/api/v0/dag/get", func(w http.ResponseWriter, r *http.Request) {
		cid := r.URL.Query().Get("arg")
		codec := r.URL.Query().Get("output-codec")
		if codec == "" {
			codec = "dag-json"
		}
		url := fmt.Sprintf("/ipfs/%s?format=%s", cid, codec)
		http.Redirect(w, r, url, http.StatusFound)
	})

	mux.HandleFunc("/api/v0/dag/export", func(w http.ResponseWriter, r *http.Request) {
		cid := r.URL.Query().Get("arg")
		url := fmt.Sprintf("/ipfs/%s?format=car", cid)
		http.Redirect(w, r, url, http.StatusFound)
	})

	mux.HandleFunc("/api/v0/block/get", func(w http.ResponseWriter, r *http.Request) {
		cid := r.URL.Query().Get("arg")
		url := fmt.Sprintf("/ipfs/%s?format=raw", cid)
		http.Redirect(w, r, url, http.StatusFound)
	})

	// Endpoints that have high traffic volume. We will keep redirecting these
	// for now to Kubo endpoints that are able to handle these requests.
	s := rand.NewSource(time.Now().Unix())
	rand := rand.New(s)
	redirectToKubo := func(w http.ResponseWriter, r *http.Request) {
		// Naively choose one of the Kubo RPC clients.
		endpoint := endpoints[rand.Intn(len(endpoints))]
		http.Redirect(w, r, endpoint+r.URL.Path+"?"+r.URL.RawQuery, http.StatusFound)
	}

	mux.HandleFunc("/api/v0/name/resolve", redirectToKubo)
	mux.HandleFunc("/api/v0/resolve", redirectToKubo)
	mux.HandleFunc("/api/v0/dag/resolve", redirectToKubo)
	mux.HandleFunc("/api/v0/dns", redirectToKubo)

	// Remaining requests to the API receive a 501, as well as an explanation.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		w.Write([]byte("The /api/v0 Kubo RPC is now discontinued on this server as it is not part of the gateway specification. If you need this API, please self-host a Kubo instance yourself: https://docs.ipfs.tech/install/command-line/"))
	})

	return mux
}
