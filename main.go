package main

import (
	"flag"

	"github.com/hedtahr/grepfunc/server"
	"github.com/hedtahr/grepfunc/tools/counttokens"
	"github.com/hedtahr/grepfunc/tools/deletesymbol"

	"github.com/hedtahr/grepfunc/tools/filestats"
	"github.com/hedtahr/grepfunc/tools/filesymbols"
	"github.com/hedtahr/grepfunc/tools/findrelated"
	"github.com/hedtahr/grepfunc/tools/findsymbol"
	"github.com/hedtahr/grepfunc/tools/gitcontext"
	"github.com/hedtahr/grepfunc/tools/grepcontext"
	"github.com/hedtahr/grepfunc/tools/grepdead"
	"github.com/hedtahr/grepfunc/tools/grepfunc"
	"github.com/hedtahr/grepfunc/tools/grepimports"
	"github.com/hedtahr/grepfunc/tools/greprefs"
	"github.com/hedtahr/grepfunc/tools/grepreplace"
	"github.com/hedtahr/grepfunc/tools/grepstruct"
	"github.com/hedtahr/grepfunc/tools/memory"
	"github.com/hedtahr/grepfunc/tools/movesymbol"
	"github.com/hedtahr/grepfunc/tools/multiread"
	"github.com/hedtahr/grepfunc/tools/patchedit"

	"github.com/hedtahr/grepfunc/tools/renamesymbol"
	"github.com/hedtahr/grepfunc/tools/symbolat"
)

func main() {
	projectRoot := flag.String("project-root", "", "override project root directory")
	flag.Parse()
	if *projectRoot != "" {
		server.ProjectRoot = *projectRoot
	}

	s := server.New("patch-file", "0.9.0")
	s.Register(patchedit.Tool, patchedit.Handle)
	s.Register(grepfunc.Tool, grepfunc.Handle)
	s.Register(grepstruct.Tool, grepstruct.Handle)
	s.Register(grepcontext.Tool, grepcontext.Handle)

	s.Register(findrelated.Tool, findrelated.Handle)
	s.Register(findsymbol.Tool, findsymbol.Handle)
	s.Register(memory.Tool, memory.Handle)
	s.Register(filesymbols.Tool, filesymbols.Handle)
	s.Register(filestats.Tool, filestats.Handle)
	s.Register(grepimports.Tool, grepimports.Handle)
	s.Register(greprefs.Tool, greprefs.Handle)
	s.Register(patchedit.BatchTool, patchedit.BatchHandle)

	s.Register(gitcontext.GitTool, gitcontext.GitHandle)
	s.Register(counttokens.Tool, counttokens.Handle)
	s.Register(grepdead.Tool, grepdead.Handle)
	s.Register(deletesymbol.Tool, deletesymbol.Handle)
	s.Register(renamesymbol.Tool, renamesymbol.Handle)
	s.Register(multiread.Tool, multiread.Handle)
	s.Register(symbolat.Tool, symbolat.Handle)
	s.Register(movesymbol.Tool, movesymbol.Handle)
	s.Register(grepreplace.Tool, grepreplace.Handle)
	s.Run()
}
