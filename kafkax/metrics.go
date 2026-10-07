package kafkax

import (
	"net"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"

	"github.com/sezznaw/devkit-common/metricsx"
)

// metricsHook is the franz-go side of the Kafka metrics: records written and
// read per topic and broker connections, into the process registry. The
// event counters (published, handled, dlq) are recorded by Publish and
// handle, where the outcome is known. One hook for every client of the
// process; the vectors live in metricsx and are registered once.
type metricsHook struct{}

var (
	_ kgo.HookBrokerConnect       = metricsHook{}
	_ kgo.HookProduceBatchWritten = metricsHook{}
	_ kgo.HookFetchBatchRead      = metricsHook{}
)

func (metricsHook) OnBrokerConnect(_ kgo.BrokerMetadata, _ time.Duration, _ net.Conn, err error) {
	status := "ok"
	if err != nil {
		status = "error"
	}
	metricsx.KafkaBrokerConnects.WithLabelValues(status).Inc()
}

func (metricsHook) OnProduceBatchWritten(_ kgo.BrokerMetadata, topic string, _ int32, m kgo.ProduceBatchMetrics) {
	metricsx.KafkaRecordsProduced.WithLabelValues(topic).Add(float64(m.NumRecords))
}

func (metricsHook) OnFetchBatchRead(_ kgo.BrokerMetadata, topic string, _ int32, m kgo.FetchBatchMetrics) {
	metricsx.KafkaRecordsFetched.WithLabelValues(topic).Add(float64(m.NumRecords))
}
