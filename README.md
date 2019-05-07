Connect two Go (game) engines via [Go Text Protocol](https://www.lysator.liu.se/~gunnar/gtp/gtp2-spec-draft2/gtp2-spec.html) (GTP).

# Features

* Plays multiple games with alternating colours
* Crash detection
* Legality checks
* Match resumption
* Timeouts
* Automatic SGF saving

# Notes

* Control whether the engines are restarted between games with the `restart` option. We try to send the GTP command `clear_cache` to all engines anyway, but this was added to Leela Zero only after 0.17. Without it LZ may reuse its cached data, which can only be prevented by restarting.
* The `passing_wins` option is a cheap hack to allow LZ test matches to end early; the first engine to pass is considered the winner (this is usually correct).
* Otherwise, we currently do not try to calculate the score if the game ends due to 2 passes.
* Ongoing match results are saved directly into the config file, allowing match resumption.
