module github.com/antresol-app/antresol-addon-trailers

go 1.25

toolchain go1.27.1

// go.sum keeps addonkit v0.3.6's two h1 lines (the build from the addon's own
// repository checks them); in the core repository the replace makes them
// unused, so go mod tidy there would drop them
require github.com/antresol-app/antresol/addonkit v0.3.6
