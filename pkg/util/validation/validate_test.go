package validation

import (
	"net/http"
	"strings"
	"testing"

	"github.com/go-kit/log"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/prometheus/common/model"
	"github.com/prometheus/prometheus/model/histogram"
	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/tsdb/tsdbutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/weaveworks/common/httpgrpc"

	_ "github.com/cortexproject/cortex/pkg/cortex/configinit"
	"github.com/cortexproject/cortex/pkg/cortexpb"
	util_log "github.com/cortexproject/cortex/pkg/util/log"
)

func TestValidateLabels(t *testing.T) {
	cfg := new(Limits)
	userID := "testUser"

	reg := prometheus.NewRegistry()
	validateMetrics := NewValidateMetrics(reg)

	cfg.MaxLabelValueLength = 25
	cfg.MaxLabelNameLength = 25
	cfg.MaxLabelNamesPerSeries = 2
	cfg.MaxLabelsSizeBytes = 90
	cfg.EnforceMetricName = true
	cfg.LimitsPerLabelSet = []LimitsPerLabelSet{
		{
			Limits: LimitsPerLabelSetEntry{MaxSeries: 0},
			LabelSet: labels.FromMap(map[string]string{
				model.MetricNameLabel: "foo",
			}),
			Hash: 0,
		},
		// Default partition
		{
			Limits:   LimitsPerLabelSetEntry{MaxSeries: 0},
			LabelSet: labels.EmptyLabels(),
			Hash:     1,
		},
	}

	for _, c := range []struct {
		metric                  model.Metric
		skipLabelNameValidation bool
		err                     error
	}{
		{
			map[model.LabelName]model.LabelValue{},
			false,
			newNoMetricNameError(),
		},
		{
			map[model.LabelName]model.LabelValue{model.MetricNameLabel: " "},
			false,
			newInvalidMetricNameError(" "),
		},
		{
			map[model.LabelName]model.LabelValue{model.MetricNameLabel: "valid", "foo ": "bar"},
			false,
			newInvalidLabelError([]cortexpb.LabelAdapter{
				{Name: model.MetricNameLabel, Value: "valid"},
				{Name: "foo ", Value: "bar"},
			}, "foo "),
		},
		{
			map[model.LabelName]model.LabelValue{model.MetricNameLabel: "valid"},
			false,
			nil,
		},
		{
			map[model.LabelName]model.LabelValue{model.MetricNameLabel: "badLabelName", "this_is_a_really_really_long_name_that_should_cause_an_error": "test_value_please_ignore"},
			false,
			newLabelNameTooLongError([]cortexpb.LabelAdapter{
				{Name: model.MetricNameLabel, Value: "badLabelName"},
				{Name: "this_is_a_really_really_long_name_that_should_cause_an_error", Value: "test_value_please_ignore"},
			}, "this_is_a_really_really_long_name_that_should_cause_an_error", cfg.MaxLabelNameLength),
		},
		{
			map[model.LabelName]model.LabelValue{model.MetricNameLabel: "badLabelValue", "much_shorter_name": "test_value_please_ignore_no_really_nothing_to_see_here"},
			false,
			newLabelValueTooLongError([]cortexpb.LabelAdapter{
				{Name: model.MetricNameLabel, Value: "badLabelValue"},
				{Name: "much_shorter_name", Value: "test_value_please_ignore_no_really_nothing_to_see_here"},
			}, "much_shorter_name", "test_value_please_ignore_no_really_nothing_to_see_here", cfg.MaxLabelValueLength),
		},
		{
			map[model.LabelName]model.LabelValue{model.MetricNameLabel: "foo", "bar": "baz", "blip": "blop"},
			false,
			newTooManyLabelsError([]cortexpb.LabelAdapter{
				{Name: model.MetricNameLabel, Value: "foo"},
				{Name: "bar", Value: "baz"},
				{Name: "blip", Value: "blop"},
			}, 2),
		},
		{
			map[model.LabelName]model.LabelValue{model.MetricNameLabel: "exactly_twenty_five_chars", "exactly_twenty_five_chars": "exactly_twenty_five_chars"},
			false,
			labelSizeBytesExceededError([]cortexpb.LabelAdapter{
				{Name: model.MetricNameLabel, Value: "exactly_twenty_five_chars"},
				{Name: "exactly_twenty_five_chars", Value: "exactly_twenty_five_chars"},
			}, 91, cfg.MaxLabelsSizeBytes),
		},
		{
			map[model.LabelName]model.LabelValue{model.MetricNameLabel: "foo", "invalid%label&name": "bar"},
			true,
			nil,
		},
	} {
		err := ValidateLabels(validateMetrics, cfg, userID, cortexpb.FromMetricsToLabelAdapters(c.metric), c.skipLabelNameValidation)
		assert.Equal(t, c.err, err, "wrong error")
	}

	validateMetrics.DiscardedSamples.WithLabelValues("random reason", "different user").Inc()

	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_discarded_samples_total The total number of samples that were discarded.
			# TYPE cortex_discarded_samples_total counter
			cortex_discarded_samples_total{reason="label_invalid",user="testUser"} 1
			cortex_discarded_samples_total{reason="label_name_too_long",user="testUser"} 1
			cortex_discarded_samples_total{reason="label_value_too_long",user="testUser"} 1
			cortex_discarded_samples_total{reason="max_label_names_per_series",user="testUser"} 1
			cortex_discarded_samples_total{reason="metric_name_invalid",user="testUser"} 1
			cortex_discarded_samples_total{reason="missing_metric_name",user="testUser"} 1
			cortex_discarded_samples_total{reason="labels_size_bytes_exceeded",user="testUser"} 1

			cortex_discarded_samples_total{reason="random reason",user="different user"} 1
	`), "cortex_discarded_samples_total"))

	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_label_size_bytes The combined size in bytes of all labels and label values for a time series.
			# TYPE cortex_label_size_bytes histogram
			cortex_label_size_bytes_bucket{user="testUser",le="+Inf"} 3
			cortex_label_size_bytes_sum{user="testUser"} 148
			cortex_label_size_bytes_count{user="testUser"} 3
	`), "cortex_label_size_bytes"))

	DeletePerUserValidationMetrics(validateMetrics, userID, util_log.Logger)

	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_discarded_samples_total The total number of samples that were discarded.
			# TYPE cortex_discarded_samples_total counter
			cortex_discarded_samples_total{reason="random reason",user="different user"} 1
	`), "cortex_discarded_samples_total"))
}

func TestValidateExemplars(t *testing.T) {
	userID := "testUser"
	reg := prometheus.NewRegistry()
	validateMetrics := NewValidateMetrics(reg)
	invalidExemplars := []cortexpb.Exemplar{
		{
			// Missing labels
			Labels: nil,
		},
		{
			// Invalid timestamp
			Labels: []cortexpb.LabelAdapter{{Name: "foo", Value: "bar"}},
		},
		{
			// Combined labelset too long
			Labels:      []cortexpb.LabelAdapter{{Name: "foo", Value: strings.Repeat("0", 126)}},
			TimestampMs: 1000,
		},
	}

	for _, ie := range invalidExemplars {
		err := ValidateExemplar(validateMetrics, userID, []cortexpb.LabelAdapter{}, ie)
		assert.NotNil(t, err)
	}

	validateMetrics.DiscardedExemplars.WithLabelValues("random reason", "different user").Inc()

	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_discarded_exemplars_total The total number of exemplars that were discarded.
			# TYPE cortex_discarded_exemplars_total counter
			cortex_discarded_exemplars_total{reason="exemplar_labels_missing",user="testUser"} 1
			cortex_discarded_exemplars_total{reason="exemplar_labels_too_long",user="testUser"} 1
			cortex_discarded_exemplars_total{reason="exemplar_timestamp_invalid",user="testUser"} 1

			cortex_discarded_exemplars_total{reason="random reason",user="different user"} 1
		`), "cortex_discarded_exemplars_total"))

	// Delete test user and verify only different remaining
	DeletePerUserValidationMetrics(validateMetrics, userID, util_log.Logger)
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_discarded_exemplars_total The total number of exemplars that were discarded.
			# TYPE cortex_discarded_exemplars_total counter
			cortex_discarded_exemplars_total{reason="random reason",user="different user"} 1
	`), "cortex_discarded_exemplars_total"))
}

func TestValidateMetadata(t *testing.T) {
	cfg := new(Limits)
	cfg.EnforceMetadataMetricName = true
	cfg.MaxMetadataLength = 22
	reg := prometheus.NewRegistry()
	validateMetrics := NewValidateMetrics(reg)
	userID := "testUser"

	for _, c := range []struct {
		desc     string
		metadata *cortexpb.MetricMetadata
		err      error
	}{
		{
			"with a valid config",
			&cortexpb.MetricMetadata{MetricFamilyName: "go_goroutines", Type: cortexpb.COUNTER, Help: "Number of goroutines.", Unit: ""},
			nil,
		},
		{
			"with no metric name",
			&cortexpb.MetricMetadata{MetricFamilyName: "", Type: cortexpb.COUNTER, Help: "Number of goroutines.", Unit: ""},
			httpgrpc.Errorf(http.StatusBadRequest, "metadata missing metric name"),
		},
		{
			"with a long metric name",
			&cortexpb.MetricMetadata{MetricFamilyName: "go_goroutines_and_routines_and_routines", Type: cortexpb.COUNTER, Help: "Number of goroutines.", Unit: ""},
			httpgrpc.Errorf(http.StatusBadRequest, "metadata 'METRIC_NAME' value too long: \"go_goroutines_and_routines_and_routines\" metric \"go_goroutines_and_routines_and_routines\""),
		},
		{
			"with a long help",
			&cortexpb.MetricMetadata{MetricFamilyName: "go_goroutines", Type: cortexpb.COUNTER, Help: "Number of goroutines that currently exist.", Unit: ""},
			httpgrpc.Errorf(http.StatusBadRequest, "metadata 'HELP' value too long: \"Number of goroutines that currently exist.\" metric \"go_goroutines\""),
		},
		{
			"with a long unit",
			&cortexpb.MetricMetadata{MetricFamilyName: "go_goroutines", Type: cortexpb.COUNTER, Help: "Number of goroutines.", Unit: "a_made_up_unit_that_is_really_long"},
			httpgrpc.Errorf(http.StatusBadRequest, "metadata 'UNIT' value too long: \"a_made_up_unit_that_is_really_long\" metric \"go_goroutines\""),
		},
	} {
		t.Run(c.desc, func(t *testing.T) {
			err := ValidateMetadata(validateMetrics, cfg, userID, c.metadata)
			assert.Equal(t, c.err, err, "wrong error")
		})
	}

	validateMetrics.DiscardedMetadata.WithLabelValues("random reason", "different user").Inc()

	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_discarded_metadata_total The total number of metadata that were discarded.
			# TYPE cortex_discarded_metadata_total counter
			cortex_discarded_metadata_total{reason="help_too_long",user="testUser"} 1
			cortex_discarded_metadata_total{reason="metric_name_too_long",user="testUser"} 1
			cortex_discarded_metadata_total{reason="missing_metric_name",user="testUser"} 1
			cortex_discarded_metadata_total{reason="unit_too_long",user="testUser"} 1

			cortex_discarded_metadata_total{reason="random reason",user="different user"} 1
	`), "cortex_discarded_metadata_total"))

	DeletePerUserValidationMetrics(validateMetrics, userID, util_log.Logger)

	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_discarded_metadata_total The total number of metadata that were discarded.
			# TYPE cortex_discarded_metadata_total counter
			cortex_discarded_metadata_total{reason="random reason",user="different user"} 1
	`), "cortex_discarded_metadata_total"))
}

func TestValidateLabelOrder(t *testing.T) {
	cfg := new(Limits)
	cfg.MaxLabelNameLength = 10
	cfg.MaxLabelNamesPerSeries = 10
	cfg.MaxLabelValueLength = 10
	reg := prometheus.NewRegistry()
	validateMetrics := NewValidateMetrics(reg)
	userID := "testUser"

	actual := ValidateLabels(validateMetrics, cfg, userID, []cortexpb.LabelAdapter{
		{Name: model.MetricNameLabel, Value: "m"},
		{Name: "b", Value: "b"},
		{Name: "a", Value: "a"},
	}, false)
	expected := newLabelsNotSortedError([]cortexpb.LabelAdapter{
		{Name: model.MetricNameLabel, Value: "m"},
		{Name: "b", Value: "b"},
		{Name: "a", Value: "a"},
	}, "a")
	assert.Equal(t, expected, actual)

	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_discarded_samples_total The total number of samples that were discarded.
			# TYPE cortex_discarded_samples_total counter
			cortex_discarded_samples_total{reason="labels_not_sorted",user="testUser"} 1
	`), "cortex_discarded_samples_total"))
}

func TestValidateLabelDuplication(t *testing.T) {
	cfg := new(Limits)
	cfg.MaxLabelNameLength = 10
	cfg.MaxLabelNamesPerSeries = 10
	cfg.MaxLabelValueLength = 10
	reg := prometheus.NewRegistry()
	validateMetrics := NewValidateMetrics(reg)
	userID := "testUser"

	actual := ValidateLabels(validateMetrics, cfg, userID, []cortexpb.LabelAdapter{
		{Name: model.MetricNameLabel, Value: "a"},
		{Name: model.MetricNameLabel, Value: "b"},
	}, false)
	expected := newDuplicatedLabelError([]cortexpb.LabelAdapter{
		{Name: model.MetricNameLabel, Value: "a"},
		{Name: model.MetricNameLabel, Value: "b"},
	}, model.MetricNameLabel)
	assert.Equal(t, expected, actual)

	actual = ValidateLabels(validateMetrics, cfg, userID, []cortexpb.LabelAdapter{
		{Name: model.MetricNameLabel, Value: "a"},
		{Name: "a", Value: "a"},
		{Name: "a", Value: "a"},
	}, false)
	expected = newDuplicatedLabelError([]cortexpb.LabelAdapter{
		{Name: model.MetricNameLabel, Value: "a"},
		{Name: "a", Value: "a"},
		{Name: "a", Value: "a"},
	}, "a")
	assert.Equal(t, expected, actual)

	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_discarded_samples_total The total number of samples that were discarded.
			# TYPE cortex_discarded_samples_total counter
			cortex_discarded_samples_total{reason="duplicate_label_names",user="testUser"} 2
	`), "cortex_discarded_samples_total"))
}

func TestValidateNativeHistogram(t *testing.T) {
	userID := "fake"
	lbls := cortexpb.FromLabelsToLabelAdapters(labels.FromStrings("foo", "bar"))

	// Test histogram has 4 positive buckets and 4 negative buckets so 8 in total. Schema set to 1.
	h := tsdbutil.GenerateTestHistogram(0)
	fh := tsdbutil.GenerateTestFloatHistogram(0)

	histogramWithSchemaMin := tsdbutil.GenerateTestHistogram(0)
	histogramWithSchemaMin.Schema = histogram.ExponentialSchemaMin
	floatHistogramWithSchemaMin := tsdbutil.GenerateTestFloatHistogram(0)
	floatHistogramWithSchemaMin.Schema = histogram.ExponentialSchemaMin

	belowMinRangeSchemaHistogram := tsdbutil.GenerateTestFloatHistogram(0)
	belowMinRangeSchemaHistogram.Schema = -5
	exceedMaxRangeSchemaFloatHistogram := tsdbutil.GenerateTestFloatHistogram(0)
	exceedMaxRangeSchemaFloatHistogram.Schema = 20
	exceedMaxSampleSizeBytesLimitFloatHistogram := tsdbutil.GenerateTestFloatHistogram(100)

	for _, tc := range []struct {
		name                                   string
		bucketLimit                            int
		resolutionReduced                      bool
		histogram                              cortexpb.Histogram
		expectedHistogram                      cortexpb.Histogram
		expectedErr                            error
		discardedSampleLabelValue              string
		maxNativeHistogramSampleSizeBytesLimit int
	}{
		{
			name:                      "no limit, histogram",
			histogram:                 cortexpb.HistogramToHistogramProto(0, h.Copy()),
			expectedHistogram:         cortexpb.HistogramToHistogramProto(0, h.Copy()),
			discardedSampleLabelValue: nativeHistogramBucketCountLimitExceeded,
		},
		{
			name:                      "no limit, float histogram",
			histogram:                 cortexpb.FloatHistogramToHistogramProto(0, fh.Copy()),
			expectedHistogram:         cortexpb.FloatHistogramToHistogramProto(0, fh.Copy()),
			discardedSampleLabelValue: nativeHistogramBucketCountLimitExceeded,
		},
		{
			name:                      "within limit, histogram",
			bucketLimit:               8,
			histogram:                 cortexpb.HistogramToHistogramProto(0, h.Copy()),
			expectedHistogram:         cortexpb.HistogramToHistogramProto(0, h.Copy()),
			discardedSampleLabelValue: nativeHistogramBucketCountLimitExceeded,
		},
		{
			name:                      "within limit, float histogram",
			bucketLimit:               8,
			histogram:                 cortexpb.FloatHistogramToHistogramProto(0, fh.Copy()),
			expectedHistogram:         cortexpb.FloatHistogramToHistogramProto(0, fh.Copy()),
			discardedSampleLabelValue: nativeHistogramBucketCountLimitExceeded,
		},
		{
			name:                      "exceed limit and reduce resolution for 1 level, histogram",
			bucketLimit:               6,
			histogram:                 cortexpb.HistogramToHistogramProto(0, h.Copy()),
			expectedHistogram:         cortexpb.HistogramToHistogramProto(0, h.Copy().ReduceResolution(0)),
			resolutionReduced:         true,
			discardedSampleLabelValue: nativeHistogramBucketCountLimitExceeded,
		},
		{
			name:                      "exceed limit and reduce resolution for 1 level, float histogram",
			bucketLimit:               6,
			histogram:                 cortexpb.FloatHistogramToHistogramProto(0, fh.Copy()),
			expectedHistogram:         cortexpb.FloatHistogramToHistogramProto(0, fh.Copy().ReduceResolution(0)),
			resolutionReduced:         true,
			discardedSampleLabelValue: nativeHistogramBucketCountLimitExceeded,
		},
		{
			name:                      "exceed limit and reduce resolution for 2 levels, histogram",
			bucketLimit:               4,
			histogram:                 cortexpb.HistogramToHistogramProto(0, h.Copy()),
			expectedHistogram:         cortexpb.HistogramToHistogramProto(0, h.Copy().ReduceResolution(-1)),
			discardedSampleLabelValue: nativeHistogramBucketCountLimitExceeded,
		},
		{
			name:                      "exceed limit and reduce resolution for 2 levels, float histogram",
			bucketLimit:               4,
			histogram:                 cortexpb.FloatHistogramToHistogramProto(0, fh.Copy()),
			expectedHistogram:         cortexpb.FloatHistogramToHistogramProto(0, fh.Copy().ReduceResolution(-1)),
			discardedSampleLabelValue: nativeHistogramBucketCountLimitExceeded,
		},
		{
			name:                      "exceed limit but cannot reduce resolution further, histogram",
			bucketLimit:               1,
			histogram:                 cortexpb.HistogramToHistogramProto(0, h.Copy()),
			expectedErr:               newHistogramBucketLimitExceededError(lbls, 1),
			discardedSampleLabelValue: nativeHistogramBucketCountLimitExceeded,
		},
		{
			name:                      "exceed limit but cannot reduce resolution further, float histogram",
			bucketLimit:               1,
			histogram:                 cortexpb.FloatHistogramToHistogramProto(0, fh.Copy()),
			expectedErr:               newHistogramBucketLimitExceededError(lbls, 1),
			discardedSampleLabelValue: nativeHistogramBucketCountLimitExceeded,
		},
		{
			name:                      "exceed limit but cannot reduce resolution further with min schema, histogram",
			bucketLimit:               4,
			histogram:                 cortexpb.HistogramToHistogramProto(0, histogramWithSchemaMin.Copy()),
			expectedErr:               newHistogramBucketLimitExceededError(lbls, 4),
			discardedSampleLabelValue: nativeHistogramBucketCountLimitExceeded,
		},
		{
			name:                      "exceed limit but cannot reduce resolution further with min schema, float histogram",
			bucketLimit:               4,
			histogram:                 cortexpb.FloatHistogramToHistogramProto(0, floatHistogramWithSchemaMin.Copy()),
			expectedErr:               newHistogramBucketLimitExceededError(lbls, 4),
			discardedSampleLabelValue: nativeHistogramBucketCountLimitExceeded,
		},
		{
			name:                      "exceed min schema limit",
			histogram:                 cortexpb.FloatHistogramToHistogramProto(0, belowMinRangeSchemaHistogram.Copy()),
			expectedErr:               newNativeHistogramSchemaInvalidError(lbls, int(belowMinRangeSchemaHistogram.Schema)),
			discardedSampleLabelValue: nativeHistogramInvalidSchema,
		},
		{
			name:                      "exceed max schema limit",
			histogram:                 cortexpb.FloatHistogramToHistogramProto(0, exceedMaxRangeSchemaFloatHistogram.Copy()),
			expectedErr:               newNativeHistogramSchemaInvalidError(lbls, int(exceedMaxRangeSchemaFloatHistogram.Schema)),
			discardedSampleLabelValue: nativeHistogramInvalidSchema,
		},
		{
			name:                                   "exceed max sample size bytes limit",
			histogram:                              cortexpb.FloatHistogramToHistogramProto(0, exceedMaxSampleSizeBytesLimitFloatHistogram.Copy()),
			expectedErr:                            newNativeHistogramSampleSizeBytesExceededError(lbls, 126, 100),
			discardedSampleLabelValue:              nativeHistogramSampleSizeBytesExceeded,
			maxNativeHistogramSampleSizeBytesLimit: 100,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := prometheus.NewRegistry()
			validateMetrics := NewValidateMetrics(reg)
			limits := new(Limits)
			limits.MaxNativeHistogramBuckets = tc.bucketLimit
			limits.MaxNativeHistogramSampleSizeBytes = tc.maxNativeHistogramSampleSizeBytesLimit
			actualHistogram, actualErr := ValidateNativeHistogram(validateMetrics, limits, userID, lbls, tc.histogram)
			if tc.expectedErr != nil {
				require.Equal(t, tc.expectedErr, actualErr)
				require.Equal(t, float64(1), testutil.ToFloat64(validateMetrics.DiscardedSamples.WithLabelValues(tc.discardedSampleLabelValue, userID)))
				// Should never increment if error was returned
				require.Equal(t, float64(0), testutil.ToFloat64(validateMetrics.HistogramSamplesReducedResolution.WithLabelValues(userID)))

			} else {
				if tc.resolutionReduced {
					require.Equal(t, float64(1), testutil.ToFloat64(validateMetrics.HistogramSamplesReducedResolution.WithLabelValues(userID)))
				}
				require.NoError(t, actualErr)
				require.Equal(t, tc.expectedHistogram, actualHistogram)
			}
		})
	}
}

func TestValidateMetrics_UpdateSamplesDiscardedForSeries(t *testing.T) {
	reg := prometheus.NewRegistry()
	v := NewValidateMetrics(reg)
	userID := "user"
	limits := []LimitsPerLabelSet{
		{
			LabelSet: labels.FromMap(map[string]string{"foo": "bar"}),
			Hash:     0,
		},
		{
			LabelSet: labels.FromMap(map[string]string{"foo": "baz"}),
			Hash:     1,
		},
		{
			LabelSet: labels.EmptyLabels(),
			Hash:     2,
		},
	}
	v.updateSamplesDiscardedForSeries(userID, "dummy", limits, labels.FromMap(map[string]string{"foo": "bar"}), 100)
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_discarded_samples_per_labelset_total The total number of samples that were discarded for each labelset.
			# TYPE cortex_discarded_samples_per_labelset_total counter
			cortex_discarded_samples_per_labelset_total{labelset="{foo=\"bar\"}",reason="dummy",user="user"} 100
			# HELP cortex_discarded_samples_total The total number of samples that were discarded.
			# TYPE cortex_discarded_samples_total counter
			cortex_discarded_samples_total{reason="dummy",user="user"} 100
	`), "cortex_discarded_samples_total", "cortex_discarded_samples_per_labelset_total"))

	v.updateSamplesDiscardedForSeries(userID, "out-of-order", limits, labels.FromMap(map[string]string{"foo": "baz"}), 1)
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_discarded_samples_per_labelset_total The total number of samples that were discarded for each labelset.
			# TYPE cortex_discarded_samples_per_labelset_total counter
			cortex_discarded_samples_per_labelset_total{labelset="{foo=\"bar\"}",reason="dummy",user="user"} 100
			cortex_discarded_samples_per_labelset_total{labelset="{foo=\"baz\"}",reason="out-of-order",user="user"} 1
			# HELP cortex_discarded_samples_total The total number of samples that were discarded.
			# TYPE cortex_discarded_samples_total counter
			cortex_discarded_samples_total{reason="dummy",user="user"} 100
			cortex_discarded_samples_total{reason="out-of-order",user="user"} 1
	`), "cortex_discarded_samples_total", "cortex_discarded_samples_per_labelset_total"))

	// Match default partition.
	v.updateSamplesDiscardedForSeries(userID, "too-old", limits, labels.FromMap(map[string]string{"foo": "foo"}), 1)
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_discarded_samples_per_labelset_total The total number of samples that were discarded for each labelset.
			# TYPE cortex_discarded_samples_per_labelset_total counter
			cortex_discarded_samples_per_labelset_total{labelset="{foo=\"bar\"}",reason="dummy",user="user"} 100
			cortex_discarded_samples_per_labelset_total{labelset="{foo=\"baz\"}",reason="out-of-order",user="user"} 1
			cortex_discarded_samples_per_labelset_total{labelset="{}",reason="too-old",user="user"} 1
			# HELP cortex_discarded_samples_total The total number of samples that were discarded.
			# TYPE cortex_discarded_samples_total counter
			cortex_discarded_samples_total{reason="dummy",user="user"} 100
			cortex_discarded_samples_total{reason="out-of-order",user="user"} 1
			cortex_discarded_samples_total{reason="too-old",user="user"} 1
	`), "cortex_discarded_samples_total", "cortex_discarded_samples_per_labelset_total"))
}

func TestValidateMetrics_UpdateLabelSet(t *testing.T) {
	reg := prometheus.NewRegistry()
	v := NewValidateMetrics(reg)
	userID := "user"
	logger := log.NewNopLogger()
	limits := []LimitsPerLabelSet{
		{
			LabelSet: labels.FromMap(map[string]string{"foo": "bar"}),
			Hash:     0,
		},
		{
			LabelSet: labels.FromMap(map[string]string{"foo": "baz"}),
			Hash:     1,
		},
		{
			LabelSet: labels.EmptyLabels(),
			Hash:     2,
		},
	}

	v.updateSamplesDiscarded(userID, "dummy", limits, 100)
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_discarded_samples_per_labelset_total The total number of samples that were discarded for each labelset.
			# TYPE cortex_discarded_samples_per_labelset_total counter
			cortex_discarded_samples_per_labelset_total{labelset="{foo=\"bar\"}",reason="dummy",user="user"} 100
			cortex_discarded_samples_per_labelset_total{labelset="{foo=\"baz\"}",reason="dummy",user="user"} 100
			cortex_discarded_samples_per_labelset_total{labelset="{}",reason="dummy",user="user"} 100
			# HELP cortex_discarded_samples_total The total number of samples that were discarded.
			# TYPE cortex_discarded_samples_total counter
			cortex_discarded_samples_total{reason="dummy",user="user"} 100
	`), "cortex_discarded_samples_total", "cortex_discarded_samples_per_labelset_total"))

	// Remove default partition.
	userSet := map[string]map[uint64]struct {
	}{
		userID: {0: struct{}{}, 1: struct{}{}},
	}
	v.UpdateLabelSet(userSet, logger)
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_discarded_samples_per_labelset_total The total number of samples that were discarded for each labelset.
			# TYPE cortex_discarded_samples_per_labelset_total counter
			cortex_discarded_samples_per_labelset_total{labelset="{foo=\"bar\"}",reason="dummy",user="user"} 100
			cortex_discarded_samples_per_labelset_total{labelset="{foo=\"baz\"}",reason="dummy",user="user"} 100
			# HELP cortex_discarded_samples_total The total number of samples that were discarded.
			# TYPE cortex_discarded_samples_total counter
			cortex_discarded_samples_total{reason="dummy",user="user"} 100
	`), "cortex_discarded_samples_total", "cortex_discarded_samples_per_labelset_total"))

	// Remove limit 1.
	userSet = map[string]map[uint64]struct {
	}{
		userID: {0: struct{}{}},
	}
	v.UpdateLabelSet(userSet, logger)
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_discarded_samples_per_labelset_total The total number of samples that were discarded for each labelset.
			# TYPE cortex_discarded_samples_per_labelset_total counter
			cortex_discarded_samples_per_labelset_total{labelset="{foo=\"bar\"}",reason="dummy",user="user"} 100
			# HELP cortex_discarded_samples_total The total number of samples that were discarded.
			# TYPE cortex_discarded_samples_total counter
			cortex_discarded_samples_total{reason="dummy",user="user"} 100
	`), "cortex_discarded_samples_total", "cortex_discarded_samples_per_labelset_total"))

	// Remove user.
	v.UpdateLabelSet(nil, logger)
	// cortex_discarded_samples_total metric still exists as it should be cleaned up in another loop.
	require.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
			# HELP cortex_discarded_samples_total The total number of samples that were discarded.
			# TYPE cortex_discarded_samples_total counter
			cortex_discarded_samples_total{reason="dummy",user="user"} 100
	`), "cortex_discarded_samples_total", "cortex_discarded_samples_per_labelset_total"))
}
