package stages

// 合成的多 <public> 区块样本，覆盖近重复、改写、崩坏输出和「过场 + 真正答复」。
// 结构对齐历史上出过问题的形态，正文是虚构的。
var histMultiBlockReplies = map[string]string{
	"dup0926Typo": "<internal>这是一段不该发出去的核对笔记。</internal><public>蓝色主题的配置写在设置页第三节，改完要重启服务才会生效，请不要把旧缓存当成当前值。灯灭了再看一次状态页。</public><public>蓝色主题的配置写在设置页第三节，改完要重启服务才会生交，请不要把旧缓存当成当前值。灯灭了再看一次状态页。</public>\n\n@@REPLY_CONTROL@@{\"send\": true}",
	"garbled0927": "扫完啦？立刻验证：如前所述处理。扫完啦？立刻验证凭证：验证身份中……\n\n<public>凭证有效。账号 demo-user。</public>\n" +
		"<public>验证身份成功。账号 demo-user。</public>\n\n" +
		"<internal>上一个回复完全崩溃了，必须重来。不要再编造账号或路径。</internal>\n\n" +
		"<public>先坦白：刚才那份回复坏了，里面的账号和路径都不要信。实际是登录回调是空的，所以只存下一个空凭证。\n\n新的检查脚本已经准备好。扫完说一声，我马上验证真实状态（这次只报验证过的事实，不再抢跑喵）。</public>\n\n@@REPLY_CONTROL@@{\"send\": true}",
	"distinct0927Correction": "<public>抱歉，刚才只说了没真发出去……这次真的发了！</public><public>又没发出去，这次是权限被拦了。请在那边直接说一句「发一条演示帖」，我收到就能发。</public>",
	"rewrite0929": "<public>\n收到收到，嘴已焊死\n\n这条的用途只是「做行程时默认按一个人安排」。它只该影响规划，不该被当着别人念出来。\n\n以后这条只存在于行为里，不会再出现在台词里。\n</public><public>\n收到，嘴已焊死\n\n这条的用途只是「做行程时默认按一个人安排」。它只该影响规划，不该被当着别人念出来。\n\n库你就留着吧，删了这轮教训就没了。\n</public>\n@@REPLY_CONTROL@@{\"send\": true}",
	"incident1006": "<public>我先核对一下资料，这个先不猜</public><public>核对过了：手册第 3 节写的是蓝色主题，不是绿色。需要的话我可以把小节号记下来。</public>\n\n@@REPLY_CONTROL@@{\"send\": true}",
}
