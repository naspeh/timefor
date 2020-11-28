# Timefor

It's a command-line time tracker with [rofi](https://github.com/davatorium/rofi) integration.

It helps me focus on the right things, and it also reminds me to take a break.

It's a simplified version of a similar GTK based tool [tider](https://github.com/naspeh/tider).

## Installation
```
go get github.com/naspeh/timefor
```

Or just download [the binary.](https://github.com/naspeh/timefor/raw/master/timefor)

## How I use it
I run in the background
```
timefor daemon
```

I have [key-bindings][dot-sxhkd] for
```sh
# specify an activity name using rofi
timefor select

# finish current activity
timefor finish

# show today's report using notify-send
timefor report --notify

# reject current activity
timefor reject

# rename current activity
timefor select --update
```


I integrate it into [my status bar][dot-i3blocks] using
```
[timefor]
label= 
command=timefor show -t '{"full_text":"{{.FormatLabel}}", "color":"{{if .Active}}#268bd2{{else}}#586e75{{end}}"}'
format=json
interval=1
```

I always see my current activity on the screen. If `timefor` activity is work-related, then I should work. If I want to surf the internet for fun, then I should switch `timefor` activity to `@surf` or similar.

Daemon will send notification using `notify-send` after 80 minutes by default, when I see such notification I plan to
move away from my laptop in the near time.

[dot-sxhkd]: https://github.com/naspeh/dotfiles/blob/66b4b4194e881748535929b98be37aa0e25b3265/x11/sxhkdrc#L48-L49
[dot-i3blocks]: https://github.com/naspeh/dotfiles/blob/2e29db172c13fededf94208656ae52c95849af39/x11/i3/blocks.conf#L13-L17

## Reports
There is a `report` command, but it only displays today's activities like
```sh
timefor report
# Active for 00:07
#
# @go    00:07
# @test  00:05
# -----  -----
# Total  00:12
```

Today's report can be shown using `notify-send`, useful for a key-binding
```sh
timefor report --notify
```

Other reports I can get from SQLite directly
```sh
# execute sqlite3 with db file
timefor db
```

There is one main table `log` and a few useful views.

I can use predefined SQLite views for simple queries
```sql
-- today's activities grouped by name
SELECT * FROM log_daily WHERE date = date('now');

-- yesterday's activities grouped by name
SELECT * FROM log_daily WHERE date = date('now', '-1 day');
```
