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

I have key-bindings for
```sh
# specify an activity name using rofi
timefor select

# finish the current activity
timefor finish

# reject the current activity
timefor reject
```

I integrate it into my [status bar](https://github.com/vivien/i3blocks) using
```
timefor show --i3blocks
```

I always see my current activity on the screen. If `timefor` activity is work-related, then I should work. If I want to surf the internet for fun, then I should switch `timefor` activity to `@surf` or similar.

Daemon will send notification using `notify-send` after 80 minutes by default, when I see such notification I plan to
move away from my laptop in the near time.

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

# report can be shown using "notify-send", useful for key-binding
timefor report --notify
```

Other reports I can get from SQLite directly
```sh
# execute sqlite3 with db file
timefor db
```

There is one main table `log` and few useful views.

I can use predefined views for simple queries in SQLite session
```sql
-- today's activities grouped by name
SELECT * FROM log_daily WHERE date = date('now');

-- yesterday's activities grouped by name
SELECT * FROM log_daily WHERE date = date('now', '-1 day');
```
