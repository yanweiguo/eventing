/*
Copyright 2019 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package aggregator

import (
	"context"
	"fmt"
	"log"
	"net"
	"sort"
	"sync"
	"time"

	"github.com/google/mako/go/quickstore"

	"google.golang.org/grpc"

	"github.com/golang/protobuf/ptypes"
	"github.com/golang/protobuf/ptypes/timestamp"

	"knative.dev/eventing/test/common/performance/common"
	pb "knative.dev/eventing/test/common/performance/event_state"
	"knative.dev/pkg/test/mako"
)

const (
	maxRcvMsgSize         = 1024 * 1024 * 1024
	publishFailureMessage = "Publish failure"
	deliverFailureMessage = "Delivery failure"
)

// thread-safe events recording map
type eventsRecord struct {
	sync.RWMutex
	*pb.EventsRecord
}

var fatalf = log.Fatalf

type Aggregator struct {
	// thread-safe events recording maps
	sentEvents     *eventsRecord
	acceptedEvents *eventsRecord
	receivedEvents *eventsRecord

	// channel to notify the main goroutine that an events record has been received
	notifyEventsReceived chan struct{}

	// GRPC server
	listener net.Listener
	server   *grpc.Server

	publishResults bool
	makoTags       []string
	expectRecords  uint
}

func NewAggregator(listenAddr string, expectRecords uint, makoTags []string, publishResults bool) (common.Executor, error) {
	l, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to create listener: %v", err)
	}

	executor := &Aggregator{
		listener:             l,
		notifyEventsReceived: make(chan struct{}),
		makoTags:             makoTags,
		expectRecords:        expectRecords,
		publishResults:       publishResults,
	}

	// --- Create GRPC server

	s := grpc.NewServer(grpc.MaxRecvMsgSize(maxRcvMsgSize))
	pb.RegisterEventsRecorderServer(s, executor)
	executor.server = s

	// --- Initialize records maps

	executor.sentEvents = &eventsRecord{EventsRecord: &pb.EventsRecord{
		Type:   pb.EventsRecord_SENT,
		Events: make(map[string]*timestamp.Timestamp),
	}}
	executor.acceptedEvents = &eventsRecord{EventsRecord: &pb.EventsRecord{
		Type:   pb.EventsRecord_ACCEPTED,
		Events: make(map[string]*timestamp.Timestamp),
	}}
	executor.receivedEvents = &eventsRecord{EventsRecord: &pb.EventsRecord{
		Type:   pb.EventsRecord_RECEIVED,
		Events: make(map[string]*timestamp.Timestamp),
	}}

	return executor, nil
}

func (ag *Aggregator) Run(ctx context.Context) {
	var err error
	var client *mako.Client
	if ag.publishResults {
		log.Printf("Configuring Mako")

		makoClientCtx, _ := context.WithTimeout(ctx, time.Minute*10)

		client, err = mako.Setup(makoClientCtx, ag.makoTags...)
		if err != nil {
			fatalf("Failed to setup mako: %v", err)
		}

		// Use a fresh context here so that our RPC to terminate the sidecar
		// isn't subject to our timeout (or we won't shut it down when we time out)
		defer client.ShutDownFunc(context.Background())

		// Wrap fatalf in a helper or our sidecar will live forever.
		fatalf = func(f string, args ...interface{}) {
			client.ShutDownFunc(context.Background())
			log.Fatalf(f, args...)
		}

	} else {
		log.Printf("Results won't be published to mako-stub")
	}

	// --- Run GRPC events receiver
	log.Printf("Starting events recorder server")

	go func() {
		if err := ag.server.Serve(ag.listener); err != nil {
			fatalf("Failed to serve: %v", err)
		}
	}()
	go func() {
		<-ctx.Done()
		log.Printf("Terminating events recorder server")
		ag.server.GracefulStop()
	}()

	// --- Wait for all records
	log.Printf("Expecting %d events records", ag.expectRecords)
	ag.waitForEvents()
	log.Printf("Received all expected events records")

	ag.server.GracefulStop()

	// --- Publish latencies
	log.Printf("Sent count: %d", len(ag.sentEvents.Events))
	log.Printf("Accepted count: %d", len(ag.acceptedEvents.Events))
	log.Printf("Received count: %d", len(ag.receivedEvents.Events))

	log.Printf("Calculating latencies")

	// count errors
	publishErrorTimestamps := make([]time.Time, 0)
	deliverErrorTimestamps := make([]time.Time, 0)

	for sentID := range ag.sentEvents.Events {
		timestampSentProto := ag.sentEvents.Events[sentID]
		timestampSent, _ := ptypes.Timestamp(timestampSentProto)

		timestampAcceptedProto, accepted := ag.acceptedEvents.Events[sentID]
		timestampAccepted, _ := ptypes.Timestamp(timestampAcceptedProto)

		timestampReceivedProto, received := ag.receivedEvents.Events[sentID]
		timestampReceived, _ := ptypes.Timestamp(timestampReceivedProto)

		if !accepted {
			publishErrorTimestamps = append(publishErrorTimestamps, timestampSent)
			continue
		}

		if ag.publishResults {
			sendLatency := timestampAccepted.Sub(timestampSent)
			// Uncomment to get CSV directly from this container log
			//fmt.Printf("%f,%d,\n", mako.XTime(timestampSent), sendLatency.Nanoseconds())
			// TODO mako accepts float64, which imo could lead to losing some precision on local tests. It should accept int64
			if qerr := client.Quickstore.AddSamplePoint(mako.XTime(timestampSent), map[string]float64{"pl": sendLatency.Seconds()}); qerr != nil {
				log.Printf("ERROR AddSamplePoint for publish-latency: %v", qerr)
			}
		}

		if !received {
			deliverErrorTimestamps = append(deliverErrorTimestamps, timestampSent)
			continue
		}

		if ag.publishResults {
			e2eLatency := timestampReceived.Sub(timestampSent)
			// Uncomment to get CSV directly from this container log
			//fmt.Printf("%f,,%d\n", mako.XTime(timestampSent), e2eLatency.Nanoseconds())
			// TODO mako accepts float64, which imo could lead to losing some precision on local tests. It should accept int64
			if qerr := client.Quickstore.AddSamplePoint(mako.XTime(timestampSent), map[string]float64{"dl": e2eLatency.Seconds()}); qerr != nil {
				log.Printf("ERROR AddSamplePoint for deliver-latency: %v", qerr)
			}
		}
	}

	log.Printf("Publish failure count: %d", len(publishErrorTimestamps))
	log.Printf("Delivery failure count: %d", len(deliverErrorTimestamps))

	if ag.publishResults {
		log.Printf("Publishing errors")

		for _, t := range publishErrorTimestamps {
			if qerr := client.Quickstore.AddError(mako.XTime(t), publishFailureMessage); qerr != nil {
				log.Printf("ERROR AddError for publish-failure: %v", qerr)
			}
		}

		for _, t := range deliverErrorTimestamps {
			if qerr := client.Quickstore.AddError(mako.XTime(t), deliverFailureMessage); qerr != nil {
				log.Printf("ERROR AddSamplePoint for deliver-failure: %v", qerr)
			}
		}

		log.Printf("Publishing throughputs")

		sentTimestamps := eventsToTimestampsArray(&ag.sentEvents.Events)
		err = publishThpt(sentTimestamps, client.Quickstore, "st")
		if err != nil {
			log.Printf("ERROR AddSamplePoint for send-throughput: %v", err)
		}

		receivedTimestamps := eventsToTimestampsArray(&ag.receivedEvents.Events)
		err = publishThpt(receivedTimestamps, client.Quickstore, "dt")
		if err != nil {
			log.Printf("ERROR AddSamplePoint for deliver-throughput: %v", err)
		}

		if len(publishErrorTimestamps) > 2 {
			sort.Slice(publishErrorTimestamps, func(x, y int) bool { return publishErrorTimestamps[x].Before(publishErrorTimestamps[y]) })
			err = publishThpt(publishErrorTimestamps, client.Quickstore, "pet")
			if err != nil {
				log.Printf("ERROR AddSamplePoint for publish-failure-throughput: %v", err)
			}
		}

		if len(deliverErrorTimestamps) > 2 {
			sort.Slice(deliverErrorTimestamps, func(x, y int) bool { return deliverErrorTimestamps[x].Before(deliverErrorTimestamps[y]) })
			err = publishThpt(deliverErrorTimestamps, client.Quickstore, "det")
			if err != nil {
				log.Printf("ERROR AddSamplePoint for deliver-failure-throughput: %v", err)
			}
		}

		log.Printf("Publishing aggregates")

		client.Quickstore.AddRunAggregate("pe", float64(len(publishErrorTimestamps)))
		client.Quickstore.AddRunAggregate("de", float64(len(deliverErrorTimestamps)))

		log.Printf("Store to mako")

		if out, err := client.Quickstore.Store(); err != nil {
			fatalf("Failed to store data: %v\noutput: %v", err, out)
		}
	}

	log.Printf("Aggregation completed")
}

func eventsToTimestampsArray(events *map[string]*timestamp.Timestamp) []time.Time {
	values := make([]time.Time, 0, len(*events))
	for _, v := range *events {
		t, _ := ptypes.Timestamp(v)
		values = append(values, t)
	}
	sort.Slice(values, func(x, y int) bool { return values[x].Before(values[y]) })
	return values
}

func publishThpt(timestamps []time.Time, q *quickstore.Quickstore, metricName string) error {
	for i, t := range timestamps[1:] {
		var thpt uint
		j := i - 1
		for j >= 0 && t.Sub(timestamps[j]) <= time.Second {
			thpt++
			j--
		}
		if qerr := q.AddSamplePoint(mako.XTime(t), map[string]float64{metricName: float64(thpt)}); qerr != nil {
			return qerr
		}
	}
	return nil
}

// waitForEvents blocks until the expected number of events records has been received.
func (ag *Aggregator) waitForEvents() {
	for receivedRecords := uint(0); receivedRecords < ag.expectRecords; receivedRecords++ {
		<-ag.notifyEventsReceived
	}
}

// RecordSentEvents implements event_state.EventsRecorder
func (ag *Aggregator) RecordEvents(_ context.Context, in *pb.EventsRecordList) (*pb.RecordReply, error) {
	defer func() {
		ag.notifyEventsReceived <- struct{}{}
	}()

	for _, recIn := range in.Items {
		recType := recIn.GetType()

		var rec *eventsRecord

		switch recType {
		case pb.EventsRecord_SENT:
			rec = ag.sentEvents
		case pb.EventsRecord_ACCEPTED:
			rec = ag.acceptedEvents
		case pb.EventsRecord_RECEIVED:
			rec = ag.receivedEvents
		default:
			log.Printf("Ignoring events record of type %s", recType)
			continue
		}

		log.Printf("-> Recording %d %s events", uint64(len(recIn.Events)), recType)

		func() {
			rec.Lock()
			defer rec.Unlock()
			for id, t := range recIn.Events {
				if _, exists := rec.Events[id]; exists {
					log.Printf("!! Found duplicate %s event ID %s", recType, id)
					continue
				}
				rec.Events[id] = t
			}
		}()
	}

	return &pb.RecordReply{Count: uint32(len(in.Items))}, nil
}
