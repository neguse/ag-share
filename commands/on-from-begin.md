---
description: Start forwarding this session AND replay its whole history into the thread
---

The ag-share UserPromptSubmit hook did not intercept this command, which means
ag-share is not working in this session (binary missing, hook not registered
or not trusted, or the plugin is broken). Tell the user that the share toggle
did NOT take effect and that they should check the ag-share plugin
installation (`~/.config/ag-share/error.log` is the first place to look). Do
not attempt to enable sharing yourself.
