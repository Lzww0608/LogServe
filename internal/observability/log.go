package observability

import (
	"encoding/json"
	"log"
	"time"
)

func Info(event string, fields map[string]any) {
	write("info", event, fields)
}

func Error(event string, err error, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["error"] = err.Error()
	write("error", event, fields)
}

func write(level, event string, fields map[string]any) {
	if fields == nil {
		fields = map[string]any{}
	}
	fields["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	fields["level"] = level
	fields["event"] = event
	data, err := json.Marshal(fields)
	if err != nil {
		log.Printf(`{"level":"error","event":"structured_log_encode_failed","error":%q}`, err.Error())
		return
	}
	log.Print(string(data))
}
