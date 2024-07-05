package builtin

import (
	_ "github.com/longxiucai/logkit/sender/discard"
	_ "github.com/longxiucai/logkit/sender/elasticsearch"
	_ "github.com/longxiucai/logkit/sender/file"
	_ "github.com/longxiucai/logkit/sender/http"
	_ "github.com/longxiucai/logkit/sender/influxdb"
	_ "github.com/longxiucai/logkit/sender/mock"
)
