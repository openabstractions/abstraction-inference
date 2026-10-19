package inference

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	jobwire "github.com/openabstractions/abstraction-inference/go/abstraction/inference/job"
	acceptance "github.com/openabstractions/abstraction-job/go/abstraction/job/acceptance"
	"github.com/openabstractions/abstraction-job/go/acceptanceprovider"
)

// Containers as Go's sniffer reads them (net/http DetectContentType).
var (
	testMP4  = []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom")
	testWebM = []byte("\x1a\x45\xdf\xa3\x9f\x42\x86\x81\x01\x42\xf7\x81\x01\x42\x82\x84webm")
	testAVI  = []byte("RIFF\x24\x00\x00\x00AVI LIST\x00\x00\x00\x00hdrlavih")
	testPNG  = []byte("\x89PNG\r\n\x1a\n1234")
)

// TestGeneratedJobMediaFollowsTheProfile: a video job delivers MP4, WebM or
// AVI and never an image; an image batch delivers PNG, JPEG or WebP and never
// a video. The declared type must equal what the bytes sniff as.
func TestGeneratedJobMediaFollowsTheProfile(t *testing.T) {
	for _, c := range []struct {
		name    string
		profile jobwire.Profile
		data    []byte
		media   string
		want    acceptance.WorkState
	}{
		{"video-mp4", jobwire.ProfileVideo, testMP4, "video/mp4", acceptance.WorkStateComplete},
		{"video-webm", jobwire.ProfileVideo, testWebM, "video/webm", acceptance.WorkStateComplete},
		{"video-avi", jobwire.ProfileVideo, testAVI, "video/avi", acceptance.WorkStateComplete},
		{"video-png", jobwire.ProfileVideo, testPNG, "image/png", acceptance.WorkStateFailed},
		{"video-declared-webm-sniffs-mp4", jobwire.ProfileVideo, testMP4, "video/webm", acceptance.WorkStateFailed},
		{"batch-png", jobwire.ProfileImageBatch, testPNG, "image/png", acceptance.WorkStateComplete},
		{"batch-mp4", jobwire.ProfileImageBatch, testMP4, "video/mp4", acceptance.WorkStateFailed},
	} {
		t.Run(c.name, func(t *testing.T) {
			backend := &fakeDurableBackend{poll: durableJobStatus{State: durableJobSucceeded, Outputs: []durableJobOutput{{Data: c.data, MediaType: c.media}}}}
			fixture := newDurableFixture(t, backend)
			provider, err := acceptanceprovider.OpenWithExecutor(t.TempDir(), "owner", fixture.execution)
			if err != nil {
				t.Fatal(err)
			}
			submission := durableSubmission(t, provider, c.name, c.profile)
			if accepted, submitErr := provider.Bind("caller-scope").Submit(submission); submitErr != nil || accepted.Receipt == nil {
				t.Fatalf("submit %+v %v", accepted, submitErr)
			}
			stop := executeUntilCleanup(t, provider)
			defer stop()
			final := waitDurable(t, provider.BindOperations("caller-scope"), submission.Identity, true)
			if final.State != c.want {
				t.Fatalf("final %+v, want %s", final, c.want)
			}
			if c.want == acceptance.WorkStateFailed && (final.Failure == nil || final.Failure.Message != "upstream:output") {
				t.Fatalf("failure %+v", final.Failure)
			}
		})
	}
}

// TestReplicateDurableVideoAcceptsEveryVideoContainer: the replicate backend
// reads the same media rule as the executor, so a WebM or AVI result is a
// video delivery and a PNG result for a video job is refused.
func TestReplicateDurableVideoAcceptsEveryVideoContainer(t *testing.T) {
	for _, c := range []struct {
		data []byte
		ok   bool
	}{{testMP4, true}, {testWebM, true}, {testAVI, true}, {testPNG, false}} {
		body := string(c.data)
		client := &http.Client{Transport: imageRoundTrip(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}
		backend := replicateJobBackend{client: client, maxEach: 1 << 20, maxTotal: 1 << 20}
		request := jobwire.Request{Profile: jobwire.ProfileVideo, Image: jobwire.ImageRequest{Count: 1}}
		status, err := backend.status(context.Background(), replicatePrediction{ID: "prediction", Status: "succeeded", Output: []string{"https://replicate.delivery/result"}}, request)
		if c.ok && (err != nil || len(status.Outputs) != 1 || status.Outputs[0].MediaType != http.DetectContentType(c.data)) {
			t.Fatalf("%q: status %+v error %v", http.DetectContentType(c.data), status, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("%q accepted as a video", http.DetectContentType(c.data))
		}
	}
}
