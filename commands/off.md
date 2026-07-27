---
description: Stop forwarding this session to the configured chat thread
---

The ag-share UserPromptSubmit hook did not intercept this command, which means
ag-share is not working in this session (binary missing, hook not registered
or not trusted, or the plugin is broken). Tell the user that the share toggle
did NOT take effect — forwarding state is unchanged — and that they should
check the ag-share plugin installation (`~/.config/ag-share/error.log` is the
first place to look). Do not attempt to change sharing state yourself.
