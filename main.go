package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"cloud.google.com/go/storage"
	"github.com/googleapis/gax-go/v2"
	"golang.org/x/oauth2"
	"golang.org/x/sync/errgroup"
	"google.golang.org/api/option"
	// Install google-c2p resolver, which is required for direct path.
	_ "google.golang.org/grpc/xds/googledirectpath"
)

var (
	GrpcConnPoolSize    = 1
	MaxConnsPerHost     = 100
	MaxIdelConnsPerHost = 100

	NumOfWorker = 48

	NumOfReadCallPerWorker = 800

	MaxRetryDuration = 30 * time.Second

	RetryMultiplier = 2.0

	BucketName = "golang-grpc-test-princer-gcsfuse-us-central"

	ProjectName = "gcs-fuse-test"

	// ObjectNamePrefix<worker_id>ObjectNameSuffix is the object name format.
	// Here, worker id goes from <0 to NumberOfWorker.
	ObjectNamePrefix = "50mb/1_thread."
	ObjectNameSuffix = ".0"

	eG errgroup.Group
)

func CreateHttpClient(ctx context.Context, isHttp2 bool) (client *storage.Client, err error) {
	var transport *http.Transport
	// Using http1 makes the client more performant.
	if isHttp2 == false {
		transport = &http.Transport{
			MaxConnsPerHost:     MaxConnsPerHost,
			MaxIdleConnsPerHost: MaxIdelConnsPerHost,
			// This disables HTTP/2 in transport.
			TLSNextProto: make(
				map[string]func(string, *tls.Conn) http.RoundTripper,
			),
		}
	} else {
		// For http2, change in MaxConnsPerHost doesn't affect the performance.
		transport = &http.Transport{
			DisableKeepAlives: true,
			MaxConnsPerHost:   MaxConnsPerHost,
			ForceAttemptHTTP2: true,
		}
	}

	tokenSource, err := GetTokenSource(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("while generating tokenSource, %v", err)
	}

	// Custom http client for Go Client.
	httpClient := &http.Client{
		Transport: &oauth2.Transport{
			Base:   transport,
			Source: tokenSource,
		},
		Timeout: 0,
	}

	// Setting UserAgent through RoundTripper middleware
	httpClient.Transport = &userAgentRoundTripper{
		wrapped:   httpClient.Transport,
		UserAgent: "prince",
	}

	return storage.NewClient(ctx, option.WithHTTPClient(httpClient))
}

func CreateGrpcClient(ctx context.Context) (client *storage.Client, err error) {
	if err := os.Setenv("STORAGE_USE_GRPC", "gRPC"); err != nil {
		log.Fatalf("error setting grpc env var: %v", err)
	}

	if err := os.Setenv("GOOGLE_CLOUD_ENABLE_DIRECT_PATH_XDS", "true"); err != nil {
		log.Fatalf("error setting direct path env var: %v", err)
	}

	client, err = storage.NewClient(ctx, option.WithGRPCConnectionPool(GrpcConnPoolSize))

	if err := os.Unsetenv("STORAGE_USE_GRPC"); err != nil {
		log.Fatalf("error while unsetting grpc env var: %v", err)
	}

	if err := os.Unsetenv("GOOGLE_CLOUD_ENABLE_DIRECT_PATH_XDS"); err != nil {
		log.Fatalf("error while unsetting direct path env var: %v", err)
	}
	return
}

func ReadObject(ctx context.Context, workerId int, bucketHandle *storage.BucketHandle) (err error) {

	objectName := ObjectNamePrefix + strconv.Itoa(workerId) + ObjectNameSuffix

	for i := 0; i < NumOfReadCallPerWorker; i++ {
		start := time.Now()
		object := bucketHandle.Object(objectName)
		rc, err := object.NewReader(ctx)
		if err != nil {
			return fmt.Errorf("while creating reader object: %v", err)
		}

		_, err = io.Copy(io.Discard, rc)
		if err != nil {
			return fmt.Errorf("while reading and discarding content: %v", err)
		}

		duration := time.Since(start)
		fmt.Println(duration)

		rc.Close()
	}

	return
}

func main() {
	clientProtocol := flag.String("client-protocol", "http", "# of iterations")
	flag.Parse()

	ctx := context.Background()

	var client *storage.Client
	var err error
	if *clientProtocol == "http" {
		client, err = CreateHttpClient(ctx, false)
	} else {
		client, err = CreateGrpcClient(ctx)
	}

	if err != nil {
		fmt.Errorf("while creating the client: %v", err)
	}

	client.SetRetry(
		storage.WithBackoff(gax.Backoff{
			Max:        MaxRetryDuration,
			Multiplier: RetryMultiplier,
		}),
		storage.WithPolicy(storage.RetryAlways))

	bucketHandle := client.Bucket(BucketName)
	err = bucketHandle.Create(ctx, ProjectName, nil)

	if err != nil {
		fmt.Errorf("while creating the bucket: %v", err)
	}

	for i := 0; i < NumOfWorker; i++ {
		eG.Go(func() error {
			idx := i
			err = ReadObject(ctx, idx, bucketHandle)
			if err != nil {
				err = fmt.Errorf("while reading object: %w", err)
				return err
			}
			return err
		})
	}

	err = eG.Wait()

	if err == nil {
		fmt.Println("Read benchmark completed successfully!")
	} else {
		fmt.Fprintf(os.Stderr, "Error while running benchmark: %v", err)
	}
}
