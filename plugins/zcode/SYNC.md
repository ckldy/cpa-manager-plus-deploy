# ZCode plugin

`plugin/` contains production-matched Go source (version 0.6.11). `solver/` is the isolated Node solver. All high-risk features are disabled in the public template. Build: `cd plugin && go build -trimpath -buildmode=c-shared -o zcode.so .`.
