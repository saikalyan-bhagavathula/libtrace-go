package transmission

// txClient handles the transmission of events to Opsramp.
//
// Overview
//
// Create a new instance of Client.
// Set any of the public fields for which you want to override the defaults.
// Call Start() to spin up the background goroutines necessary for transmission
// Call Add(Event) to queue an event for transmission
// Ensure Stop() is called to flush all in-flight messages.

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"io/ioutil"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/facebookgo/muster"
	proxypb "github.com/honeycombio/libhoney-go/proto/proxypb"
	"github.com/klauspost/compress/zstd"
	"github.com/vmihailenco/msgpack/v5"
	"google.golang.org/grpc"
)

const (
	apiMaxBatchSize    int = 5000000 // 5MB
	apiEventSizeMax    int = 100000  // 100KB
	maxOverflowBatches int = 10
)

// Version is the build version, set by libhoney
var Version string
var Opsramptoken, OpsrampKey, OpsrampSecret, ApiEndPoint string

type Opsramptraceproxy struct {
	// how many events to collect into a batch before sending
	MaxBatchSize uint

	// how often to send off batches
	BatchTimeout time.Duration

	// how many batches can be inflight simultaneously
	MaxConcurrentBatches uint

	// how many events to allow to pile up
	// if not specified, then the work channel becomes blocking
	// and attempting to add an event to the queue can fail
	PendingWorkCapacity uint

	// whether to block or drop events when the queue fills
	BlockOnSend bool

	// whether to block or drop responses when the queue fills
	BlockOnResponse bool

	UserAgentAddition string

	// toggles compression when sending batches of events
	DisableCompression bool

	// Deprecated, synonymous with DisableCompression
	DisableGzipCompression bool

	// set true to send events with msgpack encoding
	EnableMsgpackEncoding bool

	batchMaker func() muster.Batch
	responses  chan Response

	Transport http.RoundTripper

	muster     *muster.Client
	musterLock sync.RWMutex

	Logger  Logger
	Metrics Metrics

	UseTls         bool
	UseTlsInsecure bool

	OpsrampKey	string
	OpsrampSecret string
	ApiHost 	string
}


type OpsRampAuthTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64    `json:"expires_in"`
	Scope       string `json:"scope"`
}


func (h *Opsramptraceproxy) Start() error {
	if h.Logger == nil {
		h.Logger = &nullLogger{}
	}
	h.Logger.Printf("default transmission starting")
	h.responses = make(chan Response, h.PendingWorkCapacity*2)
	if h.Metrics == nil {
		h.Metrics = &nullMetrics{}
	}
	if h.batchMaker == nil {
		h.batchMaker = func() muster.Batch {
			return &batchAgg{
				userAgentAddition: h.UserAgentAddition,
				batches:           map[string][]*Event{},
				httpClient: &http.Client{
					Transport: h.Transport,
					Timeout:   60 * time.Second,
				},
				blockOnResponse:       h.BlockOnResponse,
				responses:             h.responses,
				metrics:               h.Metrics,
				disableCompression:    h.DisableGzipCompression || h.DisableCompression,
				enableMsgpackEncoding: h.EnableMsgpackEncoding,
				logger:                h.Logger,
				useTls:                h.UseTls,
				useTlsInsecure:        h.UseTlsInsecure,
				OpsrampKey: 		   h.OpsrampKey,
				OpsrampSecret:		   h.OpsrampSecret,
				ApiHost:			   h.ApiHost,
			}
		}
	}

	fmt.Println("OpsrampKey is:  ", h.OpsrampKey)
	fmt.Println("OpsrampSecret is:  ", h.OpsrampSecret)

	OpsrampKey = h.OpsrampKey
	OpsrampSecret = h.OpsrampSecret
	ApiEndPoint =  h.ApiHost
	Opsramptoken = opsrampOauthToken()
	m := h.createMuster()
	h.muster = m
	return h.muster.Start()
}

func opsrampOauthToken() string  {


	fmt.Println("OpsrampKey:", OpsrampKey)
	fmt.Println("OpsrampSecret:", OpsrampSecret)
	url := fmt.Sprintf("%s/auth/oauth/token", strings.TrimRight(ApiEndPoint, "/"))
	fmt.Println(url)
	requestBody := strings.NewReader("client_id=" + OpsrampKey + "&client_secret=" + OpsrampSecret + "&grant_type=client_credentials")
	req, err := http.NewRequest(http.MethodPost, url, requestBody)
	fmt.Println("request error is: ", err)
	req.Header.Add("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Add("Accept", "application/json")
	req.Header.Set("Connection", "close")


	resp, err := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	fmt.Println("Response error is: ", err)

	respBody, err := ioutil.ReadAll(resp.Body)
	fmt.Println("resp.Body is ", string(respBody))
	var tokenResponse OpsRampAuthTokenResponse
	err = json.Unmarshal(respBody, &tokenResponse)
	//fmt.Println("tokenResponse", tokenResponse)
	fmt.Println("tokenResponse.AccessToken: ", tokenResponse.AccessToken)
	return tokenResponse.AccessToken
}

func (h *Opsramptraceproxy) createMuster() *muster.Client {
	m := new(muster.Client)
	m.MaxBatchSize = h.MaxBatchSize
	m.BatchTimeout = h.BatchTimeout
	m.MaxConcurrentBatches = h.MaxConcurrentBatches
	m.PendingWorkCapacity = h.PendingWorkCapacity
	m.BatchMaker = h.batchMaker
	return m
}

func (h *Opsramptraceproxy) Stop() error {
	h.Logger.Printf("Opsramptraceproxy transmission stopping")
	err := h.muster.Stop()
	close(h.responses)
	return err
}

func (h *Opsramptraceproxy) Flush() (err error) {
	// There isn't a way to flush a muster.Client directly, so we have to stop
	// the old one (which has a side-effect of flushing the data) and make a new
	// one. We start the new one and swap it with the old one so that we minimize
	// the time we hold the musterLock for.
	newMuster := h.createMuster()
	err = newMuster.Start()
	if err != nil {
		return err
	}
	h.musterLock.Lock()
	m := h.muster
	h.muster = newMuster
	h.musterLock.Unlock()
	return m.Stop()
}

// Add enqueues ev to be sent. If a Flush is in-progress, this will block until
// it completes. Similarly, if BlockOnSend is set and the pending work is more
// than the PendingWorkCapacity, this will block a Flush until more pending
// work can be enqueued.
func (h *Opsramptraceproxy) Add(ev *Event) {

	if h.tryAdd(ev) {
		h.Metrics.Increment("messages_queued")
		return
	}
	h.Metrics.Increment("queue_overflow")
	r := Response{
		Err:      errors.New("queue overflow"),
		Metadata: ev.Metadata,
	}
	h.Logger.Printf("got response code %d, error %s, and body %s",
		r.StatusCode, r.Err, string(r.Body))
	writeToResponse(h.responses, r, h.BlockOnResponse)
}

// tryAdd attempts to add ev to the underlying muster. It returns false if this
// was unsucessful because the muster queue (muster.Work) is full.
func (h *Opsramptraceproxy) tryAdd(ev *Event) bool {
	h.musterLock.RLock()
	defer h.musterLock.RUnlock()

	// Even though this queue is locked against changing h.Muster, the Work queue length
	// could change due to actions on the worker side, so make sure we only measure it once.
	qlen := len(h.muster.Work)
	h.Logger.Printf("adding event to transmission; queue length %d", qlen)
	h.Metrics.Gauge("queue_length", qlen)

	if h.BlockOnSend {
		h.muster.Work <- ev
		return true
	} else {
		select {
		case h.muster.Work <- ev:
			return true
		default:
			return false
		}
	}
}

func (h *Opsramptraceproxy) TxResponses() chan Response {
	return h.responses
}

func (h *Opsramptraceproxy) SendResponse(r Response) bool {
	if h.BlockOnResponse {
		h.responses <- r
	} else {
		select {
		case h.responses <- r:
		default:
			return true
		}
	}
	return false
}

// batchAgg is a batch aggregator - it's actually collecting what will
// eventually be one or more batches sent to the /1/batch/dataset endpoint.
type batchAgg struct {
	// map of batch key to a list of events destined for that batch
	batches map[string][]*Event
	// Used to reenque events when an initial batch is too large
	overflowBatches       map[string][]*Event
	httpClient            *http.Client
	blockOnResponse       bool
	userAgentAddition     string
	disableCompression    bool
	enableMsgpackEncoding bool

	responses chan Response
	// numEncoded       int

	metrics Metrics

	// allows manipulation of the value of "now" for testing
	testNower   nower
	testBlocker *sync.WaitGroup

	logger Logger

	useTls         bool
	useTlsInsecure bool
	OpsrampKey	string
	OpsrampSecret string
	ApiHost	string
}

// batch is a collection of events that will all be POSTed as one HTTP call
// type batch []*Event

func (b *batchAgg) Add(ev interface{}) {
	// from muster godoc: "The Batch does not need to be safe for concurrent
	// access; synchronization will be handled by the Client."
	if b.batches == nil {
		b.batches = map[string][]*Event{}
	}
	e := ev.(*Event)
	// collect separate buckets of events to send based on the trio of api/wk/ds
	// if all three of those match it's safe to send all the events in one batch
	key := fmt.Sprintf("%s_%s", e.APIHost, e.Dataset)
	b.batches[key] = append(b.batches[key], e)
}

func (b *batchAgg) enqueueResponse(resp Response) {
	if writeToResponse(b.responses, resp, b.blockOnResponse) {
		if b.testBlocker != nil {
			b.testBlocker.Done()
		}
	}
}

func (b *batchAgg) reenqueueEvents(events []*Event) {
	if b.overflowBatches == nil {
		b.overflowBatches = make(map[string][]*Event)
	}
	for _, e := range events {
		key := fmt.Sprintf("%s_%s", e.APIHost, e.Dataset)
		b.overflowBatches[key] = append(b.overflowBatches[key], e)
	}
}

func (b *batchAgg) Fire(notifier muster.Notifier) {
	defer notifier.Done()

	// send each batchKey's collection of event as a POST to /1/batch/<dataset>
	// we don't need the batch key anymore; it's done its sorting job
	for _, events := range b.batches {
		//b.fireBatch(events)
		//b.exportBatch(events)
		b.exportProtoMsgBatch(events)
	}
	// The initial batches could have had payloads that were greater than 5MB.
	// The remaining events will have overflowed into overflowBatches
	// Process these until complete. Overflow batches can also overflow, so we
	// have to prepare to process it multiple times
	overflowCount := 0
	if b.overflowBatches != nil {
		for len(b.overflowBatches) > 0 {
			// We really shouldn't get here but defensively avoid an endless
			// loop of re-enqueued events
			if overflowCount > maxOverflowBatches {
				break
			}
			overflowCount++
			// fetch the keys in this map - we can't range over the map
			// because it's possible that fireBatch will reenqueue more overflow
			// events
			keys := make([]string, len(b.overflowBatches))
			i := 0
			for k := range b.overflowBatches {
				keys[i] = k
				i++
			}

			for _, k := range keys {
				events := b.overflowBatches[k]
				// fireBatch may append more overflow events
				// so we want to clear this key before firing the batch
				delete(b.overflowBatches, k)
				//b.fireBatch(events)
				//b.exportBatch(events)
				b.exportProtoMsgBatch(events)
			}
		}
	}
}

//type httpError interface {
//	Timeout() bool
//}

func (b *batchAgg) exportProtoMsgBatch(events []*Event) {
	fmt.Println("Exporting ProtoMsg batch...")
	//start := time.Now().UTC()
	//if b.testNower != nil {
	//	start = b.testNower.Now()

	//}
	//b.metrics.Register("counterResponseErrors","counter")
	//b.metrics.Register("counterResponse20x","counter")
	if len(events) == 0 {
		// we managed to create a batch key with no events. odd. move on.
		return
	}

	var numEncoded int
	var encEvs []byte
	//var contentType string
	//contentType = "application/grpc"
	encEvs, numEncoded = b.encodeBatchProtoBuf(events)

	fmt.Println("Export protobuf batch data:", string(encEvs))

	// if we failed to encode any events skip this batch
	if numEncoded == 0 {
		return
	}
	//b.metrics.Register(d.Name+counterEnqueueErrors, "counter")
	//d.Metrics.Register(d.Name+counterResponse20x, "counter")
	//d.Metrics.Register(d.Name+counterResponseErrors, "counter")

	// get some attributes common to this entire batch up front off the first
	// valid event (some may be nil)
	var apiHost, tenantId, token, dataset string

	//var apiHost, writeKey, dataset string
	for _, ev := range events {
		if ev != nil {
			apiHost = ev.APIHost
			//writeKey = ev.APIKey
			dataset = ev.Dataset
			tenantId = ev.APITenantId
			token = ev.APIToken
			if len(token) == 0 {
				token = Opsramptoken
			}
			break
		}
	}

	fmt.Printf("\ntenantId: %v", tenantId)
	fmt.Printf("\ntoken: %v", token)

	if tenantId == "" {
		fmt.Println("Skipping as TenantId is empty")
		return
	}

	apiHost = strings.Replace(apiHost, "https://", "", -1)
	apiHost = strings.Replace(apiHost, "http://", "", -1)
	var apiHostUrl string
	if !strings.Contains(apiHost, ":") {
		apiHostUrl = apiHost + ":443"
	} else {
		apiHostUrl = apiHost
	}

	fmt.Printf("\napiHost: %v", apiHost)

	//Root Cert
	//cServer, _ := ioutil.ReadFile("/etc/ssl/certs/ca-certificates.crt")
	//cp := x509.NewCertPool()
	//if !cp.AppendCertsFromPEM(cServer) {
	//	fmt.Printf("\ncredentials: failed to append certificates")
	//}
	//
	//tlsCfg := &tls.Config{
	//	MinVersion:         tls.VersionTLS12,
	//	InsecureSkipVerify: true,
	//	ServerName:         apiHost,
	//	RootCAs:            cp,
	//}
	retryCount := 3
	for i := 0; i < retryCount; i++ {
		if i > 0 {
			b.metrics.Increment("send_retries")
		}
		var conn *grpc.ClientConn
		var err error
		if b.useTls {
			bInsecureSkip := b.useTlsInsecure

			tlsCfg := &tls.Config{
				InsecureSkipVerify: !bInsecureSkip,
			}

			tlsCreds := credentials.NewTLS(tlsCfg)
			fmt.Println("Connecting with Tls")
			opts := []grpc.DialOption{
				grpc.WithTransportCredentials(tlsCreds),
				grpc.WithUnaryInterceptor(grpcInterceptor),
			}
			conn, err = grpc.Dial(apiHostUrl, opts...)

			if err != nil {
				fmt.Printf("Could not connect: %v", err)
				b.metrics.Increment("send_errors")

				return
			}
		} else {
			fmt.Println("Connecting without Tls")
			conn, err = grpc.Dial(apiHostUrl, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				fmt.Printf("Could not connect: %v", err)
				b.metrics.Increment("send_errors")
				return
			}
		}

		//auth, _ := oauth.NewApplicationDefault(context.Background(), "")
		//conn, err := grpc.Dial(apiHost, grpc.WithPerRPCCredentials(auth))

		defer conn.Close()
		c := proxypb.NewTraceProxyServiceClient(conn)

		req := proxypb.ExportTraceProxyServiceRequest{}

		req.TenantId = tenantId
		//err = json.Unmarshal(encEvs, &req.Items)
		//if err != nil {
		//	fmt.Printf("Error: %v \n", err)
		//}

		for _, ev := range events {
			traceData := proxypb.ProxySpan{}
			traceData.Data = &proxypb.Data{}
			fmt.Printf("\nData: ", ev.Data)
			traceData.Data.TraceTraceID, _ = ev.Data["traceTraceID"].(string)
			traceData.Data.TraceParentID, _ = ev.Data["traceParentID"].(string)
			traceData.Data.TraceSpanID, _ = ev.Data["traceSpanID"].(string)
			traceData.Data.TraceLinkTraceID, _ = ev.Data["traceLinkTraceID"].(string)
			traceData.Data.TraceLinkSpanID, _ = ev.Data["traceLinkSpanID"].(string)
			traceData.Data.Type, _ = ev.Data["type"].(string)
			traceData.Data.MetaType, _ = ev.Data["metaType"].(string)
			traceData.Data.SpanName, _ = ev.Data["spanName"].(string)
			traceData.Data.SpanKind, _ = ev.Data["spanKind"].(string)
			traceData.Data.SpanNumEvents, _ = ev.Data["spanNumEvents"].(int64)
			traceData.Data.SpanNumLinks, _ = ev.Data["spanNumLinks"].(int64)
			traceData.Data.StatusCode, _ = ev.Data["statusCode"].(int64)
			traceData.Data.StatusMessage, _ = ev.Data["statusMessage"].(string)
			traceData.Data.Time, _ = ev.Data["time"].(int64)
			traceData.Data.DurationMs, _ = ev.Data["durationMs"].(float64)
			traceData.Data.StartTime, _ = ev.Data["startTime"].(int64)
			traceData.Data.EndTime, _ = ev.Data["endTime"].(int64)
			traceData.Data.Error, _ = ev.Data["error"].(bool)
			traceData.Data.FromProxy, _ = ev.Data["fromProxy"].(bool)
			traceData.Data.ParentName, _ = ev.Data["parentName"].(string)
			traceData.Timestamp = ev.Timestamp.String()

			resourceAttr, _ := ev.Data["resourceAttributes"].(map[string]interface{})
			for key, val := range resourceAttr {
				resourceAttrKeyVal := proxypb.KeyValue{}
				resourceAttrKeyVal.Key = key

				switch v := val.(type) {
				case nil:
					fmt.Println("x is nil") // here v has type interface{}
				case string:
					resourceAttrKeyVal.Value = &proxypb.AnyValue{Value: &proxypb.AnyValue_StringValue{StringValue: val.(string)}} // here v has type int
				case bool:
					resourceAttrKeyVal.Value = &proxypb.AnyValue{Value: &proxypb.AnyValue_BoolValue{BoolValue: val.(bool)}} // here v has type interface{}
				case int64:
					resourceAttrKeyVal.Value = &proxypb.AnyValue{Value: &proxypb.AnyValue_IntValue{IntValue: val.(int64)}} // here v has type interface{}
				default:
					fmt.Println("type unknown: ", v) // here v has type interface{}
				}

				traceData.Data.ResourceAttributes = append(traceData.Data.ResourceAttributes, &resourceAttrKeyVal)
			}
			spanAttr, _ := ev.Data["spanAttributes"].(map[string]interface{})
			for key, val := range spanAttr {
				spanAttrKeyVal := proxypb.KeyValue{}
				spanAttrKeyVal.Key = key
				//spanAttrKeyVal.Value = val.(*proxypb.AnyValue)

				switch v := val.(type) {
				case nil:
					fmt.Println("x is nil") // here v has type interface{}
				case string:
					spanAttrKeyVal.Value = &proxypb.AnyValue{Value: &proxypb.AnyValue_StringValue{StringValue: val.(string)}} // here v has type int
				case bool:
					spanAttrKeyVal.Value = &proxypb.AnyValue{Value: &proxypb.AnyValue_BoolValue{BoolValue: val.(bool)}} // here v has type interface{}
				case int64:
					spanAttrKeyVal.Value = &proxypb.AnyValue{Value: &proxypb.AnyValue_IntValue{IntValue: val.(int64)}} // here v has type interface{}
				default:
					fmt.Println("type unknown: ", v) // here v has type interface{}
				}

				traceData.Data.SpanAttributes = append(traceData.Data.SpanAttributes, &spanAttrKeyVal)
			}

			eventAttr, _ := ev.Data["eventAttributes"].(map[string]interface{})
			for key, val := range eventAttr {
				eventAttrKeyVal := proxypb.KeyValue{}
				eventAttrKeyVal.Key = key
				//spanAttrKeyVal.Value = val.(*proxypb.AnyValue)

				switch v := val.(type) {
				case nil:
					fmt.Println("x is nil") // here v has type interface{}
				case string:
					eventAttrKeyVal.Value = &proxypb.AnyValue{Value: &proxypb.AnyValue_StringValue{StringValue: val.(string)}} // here v has type int
				case bool:
					eventAttrKeyVal.Value = &proxypb.AnyValue{Value: &proxypb.AnyValue_BoolValue{BoolValue: val.(bool)}} // here v has type interface{}
				case int64:
					eventAttrKeyVal.Value = &proxypb.AnyValue{Value: &proxypb.AnyValue_IntValue{IntValue: val.(int64)}} // here v has type interface{}
				default:
					fmt.Println("type unknown: ", v) // here v has type interface{}
				}

				traceData.Data.EventAttributes = append(traceData.Data.EventAttributes, &eventAttrKeyVal)
			}

			req.Items = append(req.Items, &traceData)

			/*var tracedata []proxypb.Data
			err = json.Unmarshal(encEvs, &tracedata)
				if err != nil {
					fmt.Printf("Error: %v \n", err)
				} else {
					for _,trData:= range tracedata{
						traceData := proxypb.ProxySpan{}
						traceData.Data = &trData
						traceData.Time = uint64(ev.Timestamp.Unix())
						req.Items = append(req.Items, &traceData)
					}
				}
			*/

		}

		// Contact the server and print out its response.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

		//Add headers
		//md := metadata.New(map[string]string{"authorization": token, "tenantId": tenantId})
		ctx = metadata.AppendToOutgoingContext(ctx, "Authorization", token, "tenantId", tenantId, "dataset", dataset)

		defer cancel()
		r, err := c.ExportTraceProxy(ctx, &req)
		if err != nil ||  r.GetStatus() == ""   {
			fmt.Printf("could not export traces from proxy in %v try: %v", i, err)
			b.metrics.Increment("send_errors")
			//b.metrics.Increment( "counterResponseErrors")
			continue
		}else{
			b.metrics.Increment("batches_sent")
			//b.metrics.Increment("counterResponse20x")
		}

		fmt.Printf("\ntrace proxy response r.status: %s\n", r.Status)
		fmt.Printf("\ntrace proxy response: %s\n", r.String())
		fmt.Printf("\ntrace proxy response msg: %s\n", r.GetMessage())
		fmt.Printf("\ntrace proxy response status: %s\n", r.GetStatus())
		break
	}



	/*
		url, err := url.Parse(apiHost)
		if err != nil {
			end := time.Now().UTC()
			if b.testNower != nil {
				end = b.testNower.Now()
			}
			dur := end.Sub(start)
			b.metrics.Increment("send_errors")
			for _, ev := range events {
				// Pass the parsing error down responses channel for each event that
				// didn't already error during encoding
				if ev != nil {
					b.enqueueResponse(Response{
						Duration: dur / time.Duration(numEncoded),
						Metadata: ev.Metadata,
						Err:      err,
					})
				}
			}
			return
		}

		// build the HTTP request
		url.Path = path.Join(url.Path, "/1/batch", dataset)

		// sigh. dislike
		userAgent := fmt.Sprintf("libhoney-go/%s", Version)
		if b.userAgentAddition != "" {
			userAgent = fmt.Sprintf("%s %s", userAgent, strings.TrimSpace(b.userAgentAddition))
		}

		// One retry allowed for connection timeouts.
		var resp *http.Response
		for try := 0; try < 2; try++ {
			if try > 0 {
				b.metrics.Increment("send_retries")
			}

			var req *http.Request
			reqBody, zipped := buildReqReader(encEvs, !b.disableCompression)
			if reader, ok := reqBody.(*pooledReader); ok {
				// Pass the underlying bytes.Reader to http.Request so that
				// GetBody and ContentLength fields are populated on Request.
				// See https://cs.opensource.google/go/go/+/refs/tags/go1.17.5:src/net/http/request.go;l=898
				req, err = http.NewRequest("POST", url.String(), &reader.Reader)
			} else {
				req, err = http.NewRequest("POST", url.String(), reqBody)
			}
			req.Header.Set("Content-Type", contentType)
			if zipped {
				req.Header.Set("Content-Encoding", "zstd")
			}

			req.Header.Set("User-Agent", userAgent)
			//req.Header.Add("X-Opsramp-Team", writeKey)
			// send off batch!
			resp, err = b.httpClient.Do(req)
			if reader, ok := reqBody.(*pooledReader); ok {
				reader.Release()
			}

			if httpErr, ok := err.(httpError); ok && httpErr.Timeout() {
				continue
			}
			break
		}
		end := time.Now().UTC()
		if b.testNower != nil {
			end = b.testNower.Now()
		}
		dur := end.Sub(start)

		// if the entire HTTP POST failed, send a failed response for every event
		if err != nil {
			b.metrics.Increment("send_errors")
			// Pass the top-level send error down responses channel for each event
			// that didn't already error during encoding
			b.enqueueErrResponses(err, events, dur/time.Duration(numEncoded))
			// the POST failed so we're done with this batch key's worth of events
			return
		}

		// ok, the POST succeeded, let's process each individual response
		b.metrics.Increment("batches_sent")
		b.metrics.Count("messages_sent", numEncoded)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			b.metrics.Increment("send_errors")

			var err error
			var body []byte
			if resp.Header.Get("Content-Type") == "application/msgpack" {
				var errorBody interface{}
				decoder := msgpack.NewDecoder(resp.Body)
				err = decoder.Decode(&errorBody)
				if err == nil {
					body, err = json.Marshal(&errorBody)
				}
			} else {
				body, err = ioutil.ReadAll(resp.Body)
			}
			if err != nil {
				b.enqueueErrResponses(
					fmt.Errorf("Got HTTP error code but couldn't read response body: %v", err),
					events,
					dur/time.Duration(numEncoded),
				)
				return
			}
			// log if write key was rejected because of invalid Write / API key
			if resp.StatusCode == http.StatusUnauthorized {
				b.logger.Printf("APIKey '%s' was rejected. Please verify APIKey is correct.", writeKey)
			}
			for _, ev := range events {
				err := fmt.Errorf(
					"got unexpected HTTP status %d: %s",
					resp.StatusCode,
					http.StatusText(resp.StatusCode),
				)
				if ev != nil {
					b.enqueueResponse(Response{
						StatusCode: resp.StatusCode,
						Body:       body,
						Duration:   dur / time.Duration(numEncoded),
						Metadata:   ev.Metadata,
						Err:        err,
					})
				}
			}
			return
		}

		// decode the responses
		var batchResponses []Response
		if resp.Header.Get("Content-Type") == "application/msgpack" {
			err = msgpack.NewDecoder(resp.Body).Decode(&batchResponses)
		} else {
			err = json.NewDecoder(resp.Body).Decode(&batchResponses)
		}
		if err != nil {
			// if we can't decode the responses, just error out all of them
			b.metrics.Increment("response_decode_errors")
			b.enqueueErrResponses(fmt.Errorf(
				"got OK HTTP response, but couldn't read response body: %v", err),
				events,
				dur/time.Duration(numEncoded),
			)
			return
		}

		// Go through the responses and send them down the queue. If an Event
		// triggered a JSON error, it wasn't sent to the server and won't have a
		// returned response... so we have to be a bit more careful matching up
		// responses with Events.
		var eIdx int
		for _, resp := range batchResponses {
			resp.Duration = dur / time.Duration(numEncoded)
			for eIdx < len(events) && events[eIdx] == nil {
				fmt.Printf("incr, eIdx: %d, len(evs): %d\n", eIdx, len(events))
				eIdx++
			}
			if eIdx == len(events) { // just in case
				break
			}
			resp.Metadata = events[eIdx].Metadata
			b.enqueueResponse(resp)
			eIdx++
		}
	*/

}


var grpcInterceptor = func(ctx context.Context,
	method string,
	req interface{},
	reply interface{},
	cc *grpc.ClientConn,
	invoker grpc.UnaryInvoker,
	opts ...grpc.CallOption,
) error {

	ctx = metadata.NewOutgoingContext(ctx, metadata.New(map[string]string{"Authorization": Opsramptoken}))
	err := invoker(ctx, method, req, reply, cc, opts...)
	if status.Code(err) == codes.Unauthenticated {
		// renew oauth token here before retry
		Opsramptoken = opsrampOauthToken()
	}
	return err
}




/*func (b *batchAgg) exportBatch(events []*Event) {
	fmt.Println("Exporting batch..")
	start := time.Now().UTC()
	if b.testNower != nil {
		start = b.testNower.Now()
	}
	if len(events) == 0 {
		// we managed to create a batch key with no events. odd. move on.
		return
	}

	var numEncoded int
	var encEvs []byte
	var contentType string
	if b.enableMsgpackEncoding {
		contentType = "application/msgpack"
		encEvs, numEncoded = b.encodeBatchMsgp(events)
	} else {
		contentType = "application/json"
		encEvs, numEncoded = b.encodeBatchJSON(events)
	}
	fmt.Println("Export batch data:", string(encEvs))

	// if we failed to encode any events skip this batch
	if numEncoded == 0 {
		return
	}

	// get some attributes common to this entire batch up front off the first
	// valid event (some may be nil)
	var apiHost, dataset string
	for _, ev := range events {
		if ev != nil {
			apiHost = ev.APIHost
			//writeKey = ev.APIKey
			dataset = ev.Dataset
			break
		}
	}

	url, err := url.Parse(apiHost)
	if err != nil {
		end := time.Now().UTC()
		if b.testNower != nil {
			end = b.testNower.Now()
		}
		dur := end.Sub(start)
		b.metrics.Increment("send_errors")
		for _, ev := range events {
			// Pass the parsing error down responses channel for each event that
			// didn't already error during encoding
			if ev != nil {
				b.enqueueResponse(Response{
					Duration: dur / time.Duration(numEncoded),
					Metadata: ev.Metadata,
					Err:      err,
				})
			}
		}
		return
	}

	// build the HTTP request
	url.Path = path.Join(url.Path, "/1/batch", dataset)

	// sigh. dislike
	userAgent := fmt.Sprintf("libhoney-go/%s", Version)
	if b.userAgentAddition != "" {
		userAgent = fmt.Sprintf("%s %s", userAgent, strings.TrimSpace(b.userAgentAddition))
	}

	// One retry allowed for connection timeouts.
	var resp *http.Response
	for try := 0; try < 2; try++ {
		if try > 0 {
			b.metrics.Increment("send_retries")
		}

		var req *http.Request
		reqBody, zipped := buildReqReader(encEvs, !b.disableCompression)
		if reader, ok := reqBody.(*pooledReader); ok {
			// Pass the underlying bytes.Reader to http.Request so that
			// GetBody and ContentLength fields are populated on Request.
			// See https://cs.opensource.google/go/go/+/refs/tags/go1.17.5:src/net/http/request.go;l=898
			req, err = http.NewRequest("POST", url.String(), &reader.Reader)
		} else {
			req, err = http.NewRequest("POST", url.String(), reqBody)
		}
		req.Header.Set("Content-Type", contentType)
		if zipped {
			req.Header.Set("Content-Encoding", "zstd")
		}

		req.Header.Set("User-Agent", userAgent)
		//req.Header.Add("X-Opsramp-Team", writeKey)
		// send off batch!
		resp, err = b.httpClient.Do(req)
		if reader, ok := reqBody.(*pooledReader); ok {
			reader.Release()
		}

		if httpErr, ok := err.(httpError); ok && httpErr.Timeout() {
			continue
		}
		break
	}
	end := time.Now().UTC()
	if b.testNower != nil {
		end = b.testNower.Now()
	}
	dur := end.Sub(start)

	// if the entire HTTP POST failed, send a failed response for every event
	if err != nil {
		b.metrics.Increment("send_errors")
		// Pass the top-level send error down responses channel for each event
		// that didn't already error during encoding
		b.enqueueErrResponses(err, events, dur/time.Duration(numEncoded))
		// the POST failed so we're done with this batch key's worth of events
		return
	}

	// ok, the POST succeeded, let's process each individual response
	b.metrics.Increment("batches_sent")
	b.metrics.Count("messages_sent", numEncoded)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b.metrics.Increment("send_errors")

		var err error
		var body []byte
		if resp.Header.Get("Content-Type") == "application/msgpack" {
			var errorBody interface{}
			decoder := msgpack.NewDecoder(resp.Body)
			err = decoder.Decode(&errorBody)
			if err == nil {
				body, err = json.Marshal(&errorBody)
			}
		} else {
			body, err = ioutil.ReadAll(resp.Body)
		}
		if err != nil {
			b.enqueueErrResponses(
				fmt.Errorf("Got HTTP error code but couldn't read response body: %v", err),
				events,
				dur/time.Duration(numEncoded),
			)
			return
		}
		// log if write key was rejected because of invalid Write / API key
		if resp.StatusCode == http.StatusUnauthorized {
			b.logger.Printf("APIKey '%s' was rejected. Please verify APIKey is correct.", writeKey)
		}
		for _, ev := range events {
			err := fmt.Errorf(
				"got unexpected HTTP status %d: %s",
				resp.StatusCode,
				http.StatusText(resp.StatusCode),
			)
			if ev != nil {
				b.enqueueResponse(Response{
					StatusCode: resp.StatusCode,
					Body:       body,
					Duration:   dur / time.Duration(numEncoded),
					Metadata:   ev.Metadata,
					Err:        err,
				})
			}
		}
		return
	}

	// decode the responses
	var batchResponses []Response
	if resp.Header.Get("Content-Type") == "application/msgpack" {
		err = msgpack.NewDecoder(resp.Body).Decode(&batchResponses)
	} else {
		err = json.NewDecoder(resp.Body).Decode(&batchResponses)
	}
	if err != nil {
		// if we can't decode the responses, just error out all of them
		b.metrics.Increment("response_decode_errors")
		b.enqueueErrResponses(fmt.Errorf(
			"got OK HTTP response, but couldn't read response body: %v", err),
			events,
			dur/time.Duration(numEncoded),
		)
		return
	}

	// Go through the responses and send them down the queue. If an Event
	// triggered a JSON error, it wasn't sent to the server and won't have a
	// returned response... so we have to be a bit more careful matching up
	// responses with Events.
	var eIdx int
	for _, resp := range batchResponses {
		resp.Duration = dur / time.Duration(numEncoded)
		for eIdx < len(events) && events[eIdx] == nil {
			fmt.Printf("incr, eIdx: %d, len(evs): %d\n", eIdx, len(events))
			eIdx++
		}
		if eIdx == len(events) { // just in case
			break
		}
		resp.Metadata = events[eIdx].Metadata
		b.enqueueResponse(resp)
		eIdx++
	}
}

func (b *batchAgg) fireBatch(events []*Event) {
	start := time.Now().UTC()
	if b.testNower != nil {
		start = b.testNower.Now()
	}
	if len(events) == 0 {
		// we managed to create a batch key with no events. odd. move on.
		return
	}

	var numEncoded int
	var encEvs []byte
	var contentType string
	if b.enableMsgpackEncoding {
		contentType = "application/msgpack"
		encEvs, numEncoded = b.encodeBatchMsgp(events)
	} else {
		contentType = "application/json"
		encEvs, numEncoded = b.encodeBatchJSON(events)
	}
	// if we failed to encode any events skip this batch
	if numEncoded == 0 {
		return
	}

	// get some attributes common to this entire batch up front off the first
	// valid event (some may be nil)
	var apiHost, dataset string
	for _, ev := range events {
		if ev != nil {
			apiHost = ev.APIHost
			//writeKey = ev.APIKey
			dataset = ev.Dataset
			break
		}
	}

	url, err := url.Parse(apiHost)
	if err != nil {
		end := time.Now().UTC()
		if b.testNower != nil {
			end = b.testNower.Now()
		}
		dur := end.Sub(start)
		b.metrics.Increment("send_errors")
		for _, ev := range events {
			// Pass the parsing error down responses channel for each event that
			// didn't already error during encoding
			if ev != nil {
				b.enqueueResponse(Response{
					Duration: dur / time.Duration(numEncoded),
					Metadata: ev.Metadata,
					Err:      err,
				})
			}
		}
		return
	}

	// build the HTTP request
	url.Path = path.Join(url.Path, "/1/batch", dataset)

	// sigh. dislike
	userAgent := fmt.Sprintf("libhoney-go/%s", Version)
	if b.userAgentAddition != "" {
		userAgent = fmt.Sprintf("%s %s", userAgent, strings.TrimSpace(b.userAgentAddition))
	}

	// One retry allowed for connection timeouts.
	var resp *http.Response
	for try := 0; try < 2; try++ {
		if try > 0 {
			b.metrics.Increment("send_retries")
		}

		var req *http.Request
		reqBody, zipped := buildReqReader(encEvs, !b.disableCompression)
		if reader, ok := reqBody.(*pooledReader); ok {
			// Pass the underlying bytes.Reader to http.Request so that
			// GetBody and ContentLength fields are populated on Request.
			// See https://cs.opensource.google/go/go/+/refs/tags/go1.17.5:src/net/http/request.go;l=898
			req, err = http.NewRequest("POST", url.String(), &reader.Reader)
		} else {
			req, err = http.NewRequest("POST", url.String(), reqBody)
		}
		req.Header.Set("Content-Type", contentType)
		if zipped {
			req.Header.Set("Content-Encoding", "zstd")
		}

		req.Header.Set("User-Agent", userAgent)
		//req.Header.Add("X-Opsramp-Team", writeKey)
		// send off batch!
		resp, err = b.httpClient.Do(req)
		if reader, ok := reqBody.(*pooledReader); ok {
			reader.Release()
		}

		if httpErr, ok := err.(httpError); ok && httpErr.Timeout() {
			continue
		}
		break
	}
	end := time.Now().UTC()
	if b.testNower != nil {
		end = b.testNower.Now()
	}
	dur := end.Sub(start)

	// if the entire HTTP POST failed, send a failed response for every event
	if err != nil {
		b.metrics.Increment("send_errors")
		// Pass the top-level send error down responses channel for each event
		// that didn't already error during encoding
		b.enqueueErrResponses(err, events, dur/time.Duration(numEncoded))
		// the POST failed so we're done with this batch key's worth of events
		return
	}

	// ok, the POST succeeded, let's process each individual response
	b.metrics.Increment("batches_sent")
	b.metrics.Count("messages_sent", numEncoded)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b.metrics.Increment("send_errors")

		var err error
		var body []byte
		if resp.Header.Get("Content-Type") == "application/msgpack" {
			var errorBody interface{}
			decoder := msgpack.NewDecoder(resp.Body)
			err = decoder.Decode(&errorBody)
			if err == nil {
				body, err = json.Marshal(&errorBody)
			}
		} else {
			body, err = ioutil.ReadAll(resp.Body)
		}
		if err != nil {
			b.enqueueErrResponses(
				fmt.Errorf("Got HTTP error code but couldn't read response body: %v", err),
				events,
				dur/time.Duration(numEncoded),
			)
			return
		}
		// log if write key was rejected because of invalid Write / API key
		if resp.StatusCode == http.StatusUnauthorized {
			b.logger.Printf("APIKey '%s' was rejected. Please verify APIKey is correct.", writeKey)
		}
		for _, ev := range events {
			err := fmt.Errorf(
				"got unexpected HTTP status %d: %s",
				resp.StatusCode,
				http.StatusText(resp.StatusCode),
			)
			if ev != nil {
				b.enqueueResponse(Response{
					StatusCode: resp.StatusCode,
					Body:       body,
					Duration:   dur / time.Duration(numEncoded),
					Metadata:   ev.Metadata,
					Err:        err,
				})
			}
		}
		return
	}

	// decode the responses
	var batchResponses []Response
	if resp.Header.Get("Content-Type") == "application/msgpack" {
		err = msgpack.NewDecoder(resp.Body).Decode(&batchResponses)
	} else {
		err = json.NewDecoder(resp.Body).Decode(&batchResponses)
	}
	if err != nil {
		// if we can't decode the responses, just error out all of them
		b.metrics.Increment("response_decode_errors")
		b.enqueueErrResponses(fmt.Errorf(
			"got OK HTTP response, but couldn't read response body: %v", err),
			events,
			dur/time.Duration(numEncoded),
		)
		return
	}

	// Go through the responses and send them down the queue. If an Event
	// triggered a JSON error, it wasn't sent to the server and won't have a
	// returned response... so we have to be a bit more careful matching up
	// responses with Events.
	var eIdx int
	for _, resp := range batchResponses {
		resp.Duration = dur / time.Duration(numEncoded)
		for eIdx < len(events) && events[eIdx] == nil {
			fmt.Printf("incr, eIdx: %d, len(evs): %d\n", eIdx, len(events))
			eIdx++
		}
		if eIdx == len(events) { // just in case
			break
		}
		resp.Metadata = events[eIdx].Metadata
		b.enqueueResponse(resp)
		eIdx++
	}
}*/

// create the JSON for this event list manually so that we can send
// responses down the response queue for any that fail to marshal
func (b *batchAgg) encodeBatchJSON(events []*Event) ([]byte, int) {


	// track first vs. rest events for commas
	first := true
	// track how many we successfully encode for later bookkeeping
	var numEncoded int
	buf := bytes.Buffer{}
	buf.WriteByte('[')
	bytesTotal := 1
	// ok, we've got our array, let's populate it with JSON events
	for i, ev := range events {
		evByt, err := json.Marshal(ev)
		// check all our errors first in case we need to skip batching this event
		if err != nil {
			b.enqueueResponse(Response{
				Err:      err,
				Metadata: ev.Metadata,
			})
			// nil out the invalid Event so we can line up sent Events with server
			// responses if needed. don't delete to preserve slice length.
			events[i] = nil
			continue
		}
		// if the event is too large to ever send, add an error to the queue
		if len(evByt) > apiEventSizeMax {
			b.enqueueResponse(Response{
				Err:      fmt.Errorf("event exceeds max event size of %d bytes, API will not accept this event", apiEventSizeMax),
				Metadata: ev.Metadata,
			})
			events[i] = nil
			continue
		}

		bytesTotal += len(evByt)
		// count for the trailing ]
		if bytesTotal+1 > apiMaxBatchSize {
			b.reenqueueEvents(events[i:])
			break
		}

		// ok, we have valid JSON and it'll fit in this batch; add ourselves a comma and the next value
		if !first {
			buf.WriteByte(',')
			bytesTotal++
		}
		first = false
		buf.Write(evByt)
		numEncoded++
	}
	buf.WriteByte(']')
	return buf.Bytes(), numEncoded
}

// create the JSON for this event list manually so that we can send
// responses down the response queue for any that fail to marshal
func (b *batchAgg) encodeBatchProtoBuf(events []*Event) ([]byte, int) {
	// track first vs. rest events for commas

	first := true
	// track how many we successfully encode for later bookkeeping
	var numEncoded int
	buf := bytes.Buffer{}
	buf.WriteByte('[')
	bytesTotal := 1
	// ok, we've got our array, let's populate it with JSON events
	for i, ev := range events {
		evByt, err := json.Marshal(ev)
		// check all our errors first in case we need to skip batching this event
		if err != nil {
			b.enqueueResponse(Response{
				Err:      err,
				Metadata: ev.Metadata,
			})
			// nil out the invalid Event so we can line up sent Events with server
			// responses if needed. don't delete to preserve slice length.
			events[i] = nil
			continue
		}
		// if the event is too large to ever send, add an error to the queue
		if len(evByt) > apiEventSizeMax {
			b.enqueueResponse(Response{
				Err:      fmt.Errorf("event exceeds max event size of %d bytes, API will not accept this event", apiEventSizeMax),
				Metadata: ev.Metadata,
			})
			events[i] = nil
			continue
		}

		bytesTotal += len(evByt)
		// count for the trailing ]
		if bytesTotal+1 > apiMaxBatchSize {
			b.reenqueueEvents(events[i:])
			break
		}

		// ok, we have valid JSON and it'll fit in this batch; add ourselves a comma and the next value
		if !first {
			buf.WriteByte(',')
			bytesTotal++
		}
		first = false
		buf.Write(evByt)
		numEncoded++
	}
	buf.WriteByte(']')
	return buf.Bytes(), numEncoded
}

func (b *batchAgg) encodeBatchMsgp(events []*Event) ([]byte, int) {
	// Msgpack arrays need to be prefixed with the number of elements, but we
	// don't know in advance how many we'll encode, because the msgpack lib
	// doesn't do size estimation. Also, the array header is of variable size
	// based on array length, so we'll need to do some []byte shenanigans at
	// at the end of this to properly prepend the header.

	var arrayHeader [5]byte
	var numEncoded int
	var buf bytes.Buffer

	// Prepend space for largest possible msgpack array header.
	buf.Write(arrayHeader[:])
	for i, ev := range events {
		evByt, err := msgpack.Marshal(ev)
		if err != nil {
			b.enqueueResponse(Response{
				Err:      err,
				Metadata: ev.Metadata,
			})
			// nil out the invalid Event so we can line up sent Events with server
			// responses if needed. don't delete to preserve slice length.
			events[i] = nil
			continue
		}
		// if the event is too large to ever send, add an error to the queue
		if len(evByt) > apiEventSizeMax {
			b.enqueueResponse(Response{
				Err:      fmt.Errorf("event exceeds max event size of %d bytes, API will not accept this event", apiEventSizeMax),
				Metadata: ev.Metadata,
			})
			events[i] = nil
			continue
		}

		if buf.Len()+len(evByt) > apiMaxBatchSize {
			b.reenqueueEvents(events[i:])
			break
		}

		buf.Write(evByt)
		numEncoded++
	}

	headerBuf := bytes.NewBuffer(arrayHeader[:0])
	msgpack.NewEncoder(headerBuf).EncodeArrayLen(numEncoded)

	// Shenanigans. Chop off leading bytes we don't need, then copy in header.
	byts := buf.Bytes()[len(arrayHeader)-headerBuf.Len():]
	copy(byts, headerBuf.Bytes())

	return byts, numEncoded
}

func (b *batchAgg) enqueueErrResponses(err error, events []*Event, duration time.Duration) {
	for _, ev := range events {
		if ev != nil {
			b.enqueueResponse(Response{
				Err:      err,
				Duration: duration,
				Metadata: ev.Metadata,
			})
		}
	}
}

var zstdBufferPool sync.Pool

type pooledReader struct {
	bytes.Reader
	buf []byte
}

//func (r *pooledReader) Release() error {
//	// Ensure further attempts to read will return io.EOF
//	r.Reset(nil)
//	// Then reset and give up ownership of the buffer.
//	zstdBufferPool.Put(r.buf[:0])
//	r.buf = nil
//	return nil
//}

// Instantiating a new encoder is expensive, so use a global one.
// EncodeAll() is concurrency-safe.
var zstdEncoder *zstd.Encoder

func init() {
	var err error
	zstdEncoder, err = zstd.NewWriter(
		nil,
		// Compression level 2 gives a good balance of speed and compression.
		zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(2)),
		// zstd allocates 2 * GOMAXPROCS * window size, so use a small window.
		// Most Opsramp messages are smaller than this.
		zstd.WithWindowSize(1<<16),
	)
	if err != nil {
		panic(err)
	}
}

// buildReqReader returns an io.Reader and a boolean, indicating whether or not
// the underlying bytes.Reader is compressed.
//func buildReqReader(jsonEncoded []byte, compress bool) (io.Reader, bool) {
//	if compress {
//		var buf []byte
//		if found, ok := zstdBufferPool.Get().([]byte); ok {
//			buf = found[:0]
//		}
//
//		buf = zstdEncoder.EncodeAll(jsonEncoded, buf)
//		reader := pooledReader{
//			buf: buf,
//		}
//		reader.Reset(reader.buf)
//		return &reader, true
//	}
//	return bytes.NewReader(jsonEncoded), false
//}

// nower to make testing easier
type nower interface {
	Now() time.Time
}
