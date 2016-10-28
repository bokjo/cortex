package ingester

import (
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	prom_chunk "github.com/prometheus/prometheus/storage/local/chunk"
	"github.com/prometheus/prometheus/storage/metric"
	"golang.org/x/net/context"

	"github.com/weaveworks/cortex/chunk"
	"github.com/weaveworks/cortex/user"
)

type testStore struct {
	mtx sync.Mutex
	// Chunks keyed by userID.
	chunks map[string][]chunk.Chunk
}

func (s *testStore) Put(ctx context.Context, chunks []chunk.Chunk) error {
	s.mtx.Lock()
	defer s.mtx.Unlock()

	userID, err := user.GetID(ctx)
	if err != nil {
		return err
	}
	s.chunks[userID] = append(s.chunks[userID], chunks...)
	return nil
}

func (s *testStore) Get(ctx context.Context, from, through model.Time, matchers ...*metric.LabelMatcher) ([]chunk.Chunk, error) {
	return nil, nil
}

func (s *testStore) queryMatrix(userID string) model.Matrix {
	sampleStreams := map[model.Fingerprint]*model.SampleStream{}

	for _, c := range s.chunks[userID] {
		fp := c.Metric.Fingerprint()
		ss, ok := sampleStreams[fp]
		if !ok {
			ss = &model.SampleStream{
				Metric: c.Metric,
			}
			sampleStreams[fp] = ss
		}

		lc, err := prom_chunk.NewForEncoding(prom_chunk.DoubleDelta)
		if err != nil {
			panic(err)
		}
		lc.UnmarshalFromBuf(c.Data)
		it := lc.NewIterator()
		var samples []model.SamplePair
		for it.Scan() {
			samples = append(samples, it.Value())
		}

		ss.Values = append(ss.Values, samples...)
	}

	matrix := make(model.Matrix, 0, len(sampleStreams))
	for _, ss := range sampleStreams {
		matrix = append(matrix, ss)
	}
	return matrix
}

func buildTestMatrix(numSeries int, samplesPerSeries int, offset int) model.Matrix {
	m := make(model.Matrix, 0, numSeries)
	for i := 0; i < numSeries; i++ {
		ss := model.SampleStream{
			Metric: model.Metric{
				model.MetricNameLabel: model.LabelValue(fmt.Sprintf("testmetric_%d", i)),
				model.JobLabel:        "testjob",
			},
			Values: make([]model.SamplePair, 0, samplesPerSeries),
		}
		for j := 0; j < samplesPerSeries; j++ {
			ss.Values = append(ss.Values, model.SamplePair{
				Timestamp: model.Time(i + j + offset),
				Value:     model.SampleValue(i + j + offset),
			})
		}
		m = append(m, &ss)
	}
	sort.Sort(m)
	return m
}

func matrixToSamples(m model.Matrix) []*model.Sample {
	var samples []*model.Sample
	for _, ss := range m {
		for _, sp := range ss.Values {
			samples = append(samples, &model.Sample{
				Metric:    ss.Metric,
				Timestamp: sp.Timestamp,
				Value:     sp.Value,
			})
		}
	}
	return samples
}

func TestIngesterAppend(t *testing.T) {
	cfg := Config{
		FlushCheckPeriod: 99999 * time.Hour,
		MaxChunkAge:      99999 * time.Hour,
	}
	store := &testStore{
		chunks: map[string][]chunk.Chunk{},
	}
	ing, err := New(cfg, store)
	if err != nil {
		t.Fatal(err)
	}

	userIDs := []string{"1", "2", "3"}

	// Create test samples.
	testData := map[string]model.Matrix{}
	for i, userID := range userIDs {
		testData[userID] = buildTestMatrix(10, 1000, i)
	}

	// Append samples.
	for _, userID := range userIDs {
		ctx := user.WithID(context.Background(), userID)
		err = ing.Append(ctx, matrixToSamples(testData[userID]))
		if err != nil {
			t.Fatal(err)
		}
	}

	// Read samples back via ingester queries.
	for _, userID := range userIDs {
		ctx := user.WithID(context.Background(), userID)
		matcher, err := metric.NewLabelMatcher(metric.RegexMatch, model.JobLabel, ".+")
		if err != nil {
			t.Fatal(err)
		}
		res, err := ing.Query(ctx, model.Earliest, model.Latest, matcher)
		if err != nil {
			t.Fatal(err)
		}
		sort.Sort(res)

		if !reflect.DeepEqual(res, testData[userID]) {
			t.Fatalf("unexpected query result\n\nwant:\n\n%v\n\ngot:\n\n%v\n\n", testData[userID], res)
		}
	}

	// Read samples back via chunk store.
	ing.Stop()
	for _, userID := range userIDs {
		res := store.queryMatrix(userID)
		sort.Sort(res)

		if !reflect.DeepEqual(res, testData[userID]) {
			t.Fatalf("unexpected chunk store result\n\nwant:\n\n%v\n\ngot:\n\n%v\n\n", testData[userID], res)
		}
	}
}
