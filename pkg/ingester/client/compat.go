package client

import (
	"fmt"

	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/storage"
	"github.com/prometheus/prometheus/tsdb/chunkenc"
	storecache "github.com/thanos-io/thanos/pkg/store/cache"

	"github.com/cortexproject/cortex/pkg/cortexpb"
)

// ToQueryRequest builds a QueryRequest proto.
func ToQueryRequest(from, to model.Time, matchers []*labels.Matcher) (*QueryRequest, error) {
	ms, err := toLabelMatchers(matchers)
	if err != nil {
		return nil, err
	}

	return &QueryRequest{
		StartTimestampMs: int64(from),
		EndTimestampMs:   int64(to),
		Matchers:         ms,
	}, nil
}

// FromQueryRequest unpacks a QueryRequest proto.
func FromQueryRequest(cache storecache.MatchersCache, req *QueryRequest) (model.Time, model.Time, []*labels.Matcher, error) {
	matchers, err := FromLabelMatchers(cache, req.Matchers)
	if err != nil {
		return 0, 0, nil, err
	}
	from := model.Time(req.StartTimestampMs)
	to := model.Time(req.EndTimestampMs)
	return from, to, matchers, nil
}

// ToExemplarQueryRequest builds an ExemplarQueryRequest proto.
func ToExemplarQueryRequest(from, to model.Time, matchers ...[]*labels.Matcher) (*ExemplarQueryRequest, error) {
	var reqMatchers []*LabelMatchers
	for _, m := range matchers {
		ms, err := toLabelMatchers(m)
		if err != nil {
			return nil, err
		}
		reqMatchers = append(reqMatchers, &LabelMatchers{ms})
	}

	return &ExemplarQueryRequest{
		StartTimestampMs: int64(from),
		EndTimestampMs:   int64(to),
		Matchers:         reqMatchers,
	}, nil
}

// FromExemplarQueryRequest unpacks a ExemplarQueryRequest proto.
func FromExemplarQueryRequest(cache storecache.MatchersCache, req *ExemplarQueryRequest) (int64, int64, [][]*labels.Matcher, error) {
	var result [][]*labels.Matcher
	for _, m := range req.Matchers {
		matchers, err := FromLabelMatchers(cache, m.Matchers)
		if err != nil {
			return 0, 0, nil, err
		}
		result = append(result, matchers)
	}

	return req.StartTimestampMs, req.EndTimestampMs, result, nil
}

// MatrixFromSeriesSet unpacks a SeriesSet to a model.Matrix.
func MatrixFromSeriesSet(set storage.SeriesSet) (model.Matrix, error) {
	m := make(model.Matrix, 0)
	for set.Next() {
		s := set.At()
		var ss model.SampleStream
		ss.Metric = cortexpb.FromLabelAdaptersToMetric(cortexpb.FromLabelsToLabelAdapters(s.Labels()))
		ss.Values = make([]model.SamplePair, 0)
		it := s.Iterator(nil)
		for it.Next() != chunkenc.ValNone {
			t, v := it.At()
			ss.Values = append(ss.Values, model.SamplePair{
				Value:     model.SampleValue(v),
				Timestamp: model.Time(t),
			})
		}
		if it.Err() != nil {
			return nil, it.Err()
		}

		m = append(m, &ss)
	}

	return m, set.Err()
}

// ToMetricsForLabelMatchersRequest builds a MetricsForLabelMatchersRequest proto
func ToMetricsForLabelMatchersRequest(from, to model.Time, limit int, matchers []*labels.Matcher) (*MetricsForLabelMatchersRequest, error) {
	ms, err := toLabelMatchers(matchers)
	if err != nil {
		return nil, err
	}

	return &MetricsForLabelMatchersRequest{
		StartTimestampMs: int64(from),
		EndTimestampMs:   int64(to),
		MatchersSet:      []*LabelMatchers{{Matchers: ms}},
		Limit:            int64(limit),
	}, nil
}

// SeriesSetToQueryResponse builds a QueryResponse proto
func SeriesSetToQueryResponse(s storage.SeriesSet) (*QueryResponse, error) {
	result := &QueryResponse{}

	var it chunkenc.Iterator
	for s.Next() {
		series := s.At()
		samples := []cortexpb.Sample{}
		histograms := []cortexpb.Histogram{}
		it = series.Iterator(it)
		for valType := it.Next(); valType != chunkenc.ValNone; valType = it.Next() {
			switch valType {
			case chunkenc.ValFloat:
				t, v := it.At()
				samples = append(samples, cortexpb.Sample{
					TimestampMs: t,
					Value:       v,
				})
			case chunkenc.ValHistogram:
				t, v := it.AtHistogram(nil)
				histograms = append(histograms, cortexpb.HistogramToHistogramProto(t, v))
			case chunkenc.ValFloatHistogram:
				t, v := it.AtFloatHistogram(nil)
				histograms = append(histograms, cortexpb.FloatHistogramToHistogramProto(t, v))
			default:
				panic("unhandled default case")
			}
		}
		if err := it.Err(); err != nil {
			return nil, err
		}
		ts := cortexpb.TimeSeries{
			Labels: cortexpb.FromLabelsToLabelAdapters(series.Labels()),
		}
		if len(samples) > 0 {
			ts.Samples = samples
		}
		if len(histograms) > 0 {
			ts.Histograms = histograms
		}
		result.Timeseries = append(result.Timeseries, ts)
	}

	return result, s.Err()
}

// FromMetricsForLabelMatchersRequest unpacks a MetricsForLabelMatchersRequest proto
func FromMetricsForLabelMatchersRequest(cache storecache.MatchersCache, req *MetricsForLabelMatchersRequest) (model.Time, model.Time, int, [][]*labels.Matcher, error) {
	matchersSet := make([][]*labels.Matcher, 0, len(req.MatchersSet))
	for _, matchers := range req.MatchersSet {
		matchers, err := FromLabelMatchers(cache, matchers.Matchers)
		if err != nil {
			return 0, 0, 0, nil, err
		}
		matchersSet = append(matchersSet, matchers)
	}
	from := model.Time(req.StartTimestampMs)
	to := model.Time(req.EndTimestampMs)
	return from, to, int(req.Limit), matchersSet, nil
}

// ToLabelValuesRequest builds a LabelValuesRequest proto
func ToLabelValuesRequest(labelName model.LabelName, from, to model.Time, limit int, matchers []*labels.Matcher) (*LabelValuesRequest, error) {
	ms, err := toLabelMatchers(matchers)
	if err != nil {
		return nil, err
	}

	return &LabelValuesRequest{
		LabelName:        string(labelName),
		StartTimestampMs: int64(from),
		EndTimestampMs:   int64(to),
		Matchers:         &LabelMatchers{Matchers: ms},
		Limit:            int64(limit),
	}, nil
}

// FromLabelValuesRequest unpacks a LabelValuesRequest proto
func FromLabelValuesRequest(cache storecache.MatchersCache, req *LabelValuesRequest) (string, int64, int64, int, []*labels.Matcher, error) {
	var err error
	var matchers []*labels.Matcher

	if req.Matchers != nil {
		matchers, err = FromLabelMatchers(cache, req.Matchers.Matchers)
		if err != nil {
			return "", 0, 0, 0, nil, err
		}
	}

	return req.LabelName, req.StartTimestampMs, req.EndTimestampMs, int(req.Limit), matchers, nil
}

// ToLabelNamesRequest builds a LabelNamesRequest proto
func ToLabelNamesRequest(from, to model.Time, limit int, matchers []*labels.Matcher) (*LabelNamesRequest, error) {
	ms, err := toLabelMatchers(matchers)
	if err != nil {
		return nil, err
	}

	return &LabelNamesRequest{
		StartTimestampMs: int64(from),
		EndTimestampMs:   int64(to),
		Matchers:         &LabelMatchers{Matchers: ms},
		Limit:            int64(limit),
	}, nil
}

// FromLabelNamesRequest unpacks a LabelNamesRequest proto
func FromLabelNamesRequest(cache storecache.MatchersCache, req *LabelNamesRequest) (int64, int64, int, []*labels.Matcher, error) {
	var err error
	var matchers []*labels.Matcher

	if req.Matchers != nil {
		matchers, err = FromLabelMatchers(cache, req.Matchers.Matchers)
		if err != nil {
			return 0, 0, 0, nil, err
		}
	}

	return req.StartTimestampMs, req.EndTimestampMs, int(req.Limit), matchers, nil
}

func toLabelMatchers(matchers []*labels.Matcher) ([]*LabelMatcher, error) {
	result := make([]*LabelMatcher, 0, len(matchers))
	for _, matcher := range matchers {
		var mType MatchType
		switch matcher.Type {
		case labels.MatchEqual:
			mType = EQUAL
		case labels.MatchNotEqual:
			mType = NOT_EQUAL
		case labels.MatchRegexp:
			mType = REGEX_MATCH
		case labels.MatchNotRegexp:
			mType = REGEX_NO_MATCH
		default:
			return nil, fmt.Errorf("invalid matcher type")
		}
		result = append(result, &LabelMatcher{
			Type:  mType,
			Name:  matcher.Name,
			Value: matcher.Value,
		})
	}
	return result, nil
}

func FromLabelMatchers(cache storecache.MatchersCache, matchers []*LabelMatcher) ([]*labels.Matcher, error) {
	result := make([]*labels.Matcher, 0, len(matchers))
	for _, matcher := range matchers {
		m, err := cache.GetOrSet(matcher, func() (*labels.Matcher, error) {
			var mtype labels.MatchType
			switch matcher.Type {
			case EQUAL:
				mtype = labels.MatchEqual
			case NOT_EQUAL:
				mtype = labels.MatchNotEqual
			case REGEX_MATCH:
				mtype = labels.MatchRegexp
			case REGEX_NO_MATCH:
				mtype = labels.MatchNotRegexp
			default:
				return nil, fmt.Errorf("invalid matcher type")
			}
			return labels.NewMatcher(mtype, matcher.GetName(), matcher.GetValue())
		})

		if err != nil {
			return nil, err
		}

		result = append(result, m)
	}
	return result, nil
}

// FastFingerprint runs the same algorithm as Prometheus labelSetToFastFingerprint()
func FastFingerprint(ls []cortexpb.LabelAdapter) model.Fingerprint {
	if len(ls) == 0 {
		return model.Metric(nil).FastFingerprint()
	}

	var result uint64
	for _, l := range ls {
		sum := hashNew()
		sum = hashAdd(sum, l.Name)
		sum = hashAddByte(sum, model.SeparatorByte)
		sum = hashAdd(sum, l.Value)
		result ^= sum
	}
	return model.Fingerprint(result)
}

// LabelsToKeyString is used to form a string to be used as
// the hashKey. Don't print, use l.String() for printing.
func LabelsToKeyString(l labels.Labels) string {
	// We are allocating 1024, even though most series are less than 600b long.
	// But this is not an issue as this function is being inlined when called in a loop
	// and buffer allocated is a static buffer and not a dynamic buffer on the heap.
	b := make([]byte, 0, 1024)
	return string(l.Bytes(b))
}
