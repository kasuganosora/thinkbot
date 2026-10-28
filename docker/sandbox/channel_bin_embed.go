//go:build embedchannel

package botsandbox

import _ "embed"

//go:embed thinkbot-channel
var channelBinary []byte

func embeddedChannelBinary() []byte { return channelBinary }
