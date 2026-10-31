package memory

// VectorScopeKey groups explicitly owned entries into the bot's vector bucket
// without changing their original user/channel classification in storage.
// Unattributed legacy data retains its old bucket for maintenance compatibility;
// a query must still verify returned IDs against the trusted repository owner.
func VectorScopeKey(scope Scope, metadata map[string]any) string {
	if scope.Kind != ScopeBot {
		if botID, ok := metadata["bot_id"].(string); ok && botID != "" {
			return BotScope(botID).Key()
		}
	}
	return scope.Key()
}
