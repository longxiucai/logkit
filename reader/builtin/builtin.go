// Package builtin does nothing but import all builtin readers to execute their init functions.
package builtin

import (
	_ "github.com/longxiucai/logkit/reader/autofile"
	_ "github.com/longxiucai/logkit/reader/cloudtrail"
	_ "github.com/longxiucai/logkit/reader/dirx"
	_ "github.com/longxiucai/logkit/reader/elastic"
	_ "github.com/longxiucai/logkit/reader/http"
	_ "github.com/longxiucai/logkit/reader/mongo"
	_ "github.com/longxiucai/logkit/reader/redis"
	_ "github.com/longxiucai/logkit/reader/script"
	_ "github.com/longxiucai/logkit/reader/socket"
	_ "github.com/longxiucai/logkit/reader/sql"
	_ "github.com/longxiucai/logkit/reader/tailx"
)
