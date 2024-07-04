// Package builtin does nothing but import all builtin parsers to execute their init functions.
package builtin

import (
	_ "github.com/longxiucai/logkit/parser/csv"
	_ "github.com/longxiucai/logkit/parser/empty"
	_ "github.com/longxiucai/logkit/parser/grok"
	_ "github.com/longxiucai/logkit/parser/json"
	_ "github.com/longxiucai/logkit/parser/kafkarest"
	_ "github.com/longxiucai/logkit/parser/logfmt"
	_ "github.com/longxiucai/logkit/parser/mysql"
	_ "github.com/longxiucai/logkit/parser/nginx"
	_ "github.com/longxiucai/logkit/parser/qiniu"
	_ "github.com/longxiucai/logkit/parser/raw"
	_ "github.com/longxiucai/logkit/parser/syslog"
)
