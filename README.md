Connect two Go (game) engines via [Go Text Protocol](https://www.lysator.liu.se/~gunnar/gtp/gtp2-spec-draft2/gtp2-spec.html) (GTP).

# Features

* Plays multiple games with alternating colours
* Crash detection
* Legality checks
* Timeouts
* Automatic SGF saving

# Notes

* Control whether the engines are restarted between games with the "restart" option.
* The "passing_wins" option is a cheap hack to allow Leela test matches to end early; the first engine to pass is considered the winner (this is usually correct).
* Otherwise, we currently do not try to calculate the score if the game ends due to 2 passes.
