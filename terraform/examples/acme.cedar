@id("allow-by-default")
permit (principal, action, resource);

@id("tools-in-office-hours")
forbid (principal, action == Action::"call_tool", resource)
unless { context.hour >= 8 && context.hour < 18 };
