package main

import (
	"io"
	"os"
	"fmt"
	"flag"
	"path"
	"errors"
	"strings"
	"runtime"
	"encoding/csv"
	"aaronlindsay.com/go/pkg/pso2/ice"
	"aaronlindsay.com/go/pkg/pso2/text"
	"aaronlindsay.com/go/pkg/pso2/util"
	"aaronlindsay.com/go/pkg/pso2/trans"
	"aaronlindsay.com/go/pkg/pso2/trans/cmd"
)

func usage() {
	fmt.Fprintln(os.Stderr, "usage: pso2-trans [flags] pso2.db [...]")
	flag.PrintDefaults()
	os.Exit(2)
}

func complain(apath string, err error) bool {
	if err != nil {
		if apath != "" {
			fmt.Fprintf(os.Stderr, "error with file `%s`\n", apath)
		}
		fmt.Fprintln(os.Stderr, err);
		return true
	}

	return false
}

func ragequit(apath string, err error) {
	if complain(apath, err) {
		os.Exit(1)
	}
}

func main() {
	var flagTrans, flagBackup, flagOutput, flagStrip string
	var flagAidaSkits, flagAidaStrings string
	var flagImport, flagParallel int

	flag.Usage = usage
	flag.IntVar(&flagImport, "i", 0, "import files with the specified version")
	flag.IntVar(&flagParallel, "p", runtime.NumCPU() + 1, "max parallel tasks")
	flag.StringVar(&flagTrans, "t", "", "translation name (eng, story-eng, etc.)")
	flag.StringVar(&flagBackup, "b", "", "backup files to this path before modifying them")
	flag.StringVar(&flagStrip, "s", "", "write out a stripped database")
	flag.StringVar(&flagOutput, "o", "", "alternate output directory for repacked files")
	flag.StringVar(&flagAidaSkits, "aidaskits", "", "skit list file")
	flag.StringVar(&flagAidaStrings, "aidastrings", "", "translation csv file")
	flag.Parse()

	if flag.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "no database provided")
		flag.Usage()
		flag.PrintDefaults()
	}

	maxprocs := runtime.GOMAXPROCS(0)
	if maxprocs < flagParallel {
		runtime.GOMAXPROCS(flagParallel)
	}

	dbpath := flag.Arg(0)
	fmt.Fprintf(os.Stderr, "Opening database `%s`...\n", dbpath)
	db, err := trans.NewDatabase(dbpath)
	ragequit(dbpath, err)

	if flagImport != 0 {
		if flagAidaSkits != "" || flagAidaStrings != "" {
			if flagAidaSkits == "" || flagAidaStrings == "" || flagTrans == "" {
				ragequit("", errors.New("-aidaskits, -aidsstrings, and -t must all be specified together"))
			}

			fmt.Fprintln(os.Stderr, "Importing from AIDA files...")
			archiveMap := make(map[string]trans.ArchiveName)

			sf, err := os.Open(flagAidaSkits)
			ragequit(flagAidaSkits, err)

			for err != io.EOF {
				var scanArchive, scanHdr, scanGroup, scanName string
				var n int
				n, err = fmt.Fscanln(sf, &scanArchive, &scanHdr, &scanGroup, &scanName)

				if err != nil || n != 4 {
					continue
				}

				aname, err := trans.ArchiveNameFromString(scanArchive)
				if complain(scanArchive, err) {
					continue
				}

				archiveMap[scanName] = *aname
			}

			sf.Close()

			f, err := os.Open(flagAidaStrings)
			ragequit(flagAidaStrings, err)

			r := csv.NewReader(f)
			r.TrimLeadingSpace = true
			r.FieldsPerRecord = 5

			t, err := db.QueryTranslation(flagTrans)
			if t == nil {
				t, err = db.InsertTranslation(flagTrans)
				ragequit(flagTrans, err)
			}

			db.Begin()
			collisions := make(map[string]map[string]int)
			for {
				var line []string // {filename, type, zeroUnk, identifier, string}
				line, err = r.Read()

				if err != nil {
					break
				}

				line[0] = strings.Replace(line[0], "\\", "/", -1)
				archive := path.Dir(line[0])

				aname, ok := archiveMap[archive]
				if !ok {
					ragequit(archive, errors.New("unknown archive name"))
				}

				fname := path.Base(line[0])

				translation := line[4]
				identifier := line[3]

				c := collisions[line[0]]
				if c == nil {
					collisions[line[0]] = make(map[string]int)
					c = collisions[line[0]]
				}

				collision := c[identifier]
				c[identifier] = collision + 1

				a, err := db.QueryArchive(&aname)
				if complain(archive, err) {
					continue
				}

				if a == nil && complain(archive, errors.New("archive not found in database")) {
					continue
				}

				f, err := db.QueryFile(a, fname)
				if complain(fname, err) {
					continue
				}

				if f == nil && complain(archive + ": " + fname, errors.New("file not found in database")) {
					continue
				}

				s, err := db.QueryString(f, collision, identifier)
				if complain(fname + ": " + identifier, err) {
					continue
				}

				if s == nil {
					complain(fname + ": " + identifier, errors.New("string not found in database"))
					continue
				}

				if s.Value != translation {
					ts, _ := db.QueryTranslationString(t, s)
					if ts != nil {
						_, err = db.UpdateTranslationString(ts, translation)
					} else {
						_, err = db.InsertTranslationString(t, s, translation)
					}
				}
			}

			if err != io.EOF {
				ragequit(flagAidaStrings, err)
			}

			f.Close()

			db.End()

			fmt.Fprintln(os.Stderr, "Import complete!")
		} else {
			for i := 1; i < flag.NArg(); i++ {
				name := flag.Arg(i)
				aname, err := trans.ArchiveNameFromString(path.Base(name))
				if complain(name, err) {
					continue
				}

				fmt.Fprintf(os.Stderr, "Opening archive `%s`...\n", name)
				af, err := os.OpenFile(name, os.O_RDONLY, 0);
				if complain(name, err) {
					continue
				}

				archive, err := ice.NewArchive(util.BufReader(af))
				if complain(name, err) {
					af.Close()
					continue
				}

				var a *trans.Archive
				var translation *trans.Translation

				for i := 0; i < archive.GroupCount(); i++ {
					group := archive.Group(i)

					for _, file := range group.Files {
						if file.Type == "text" {
							fmt.Fprintf(os.Stderr, "Importing file `%s`...\n", file.Name)

							t, err := text.NewTextFile(file.Data)
							if complain(file.Name, err) {
								continue
							}

							if a == nil {
								a, err = db.QueryArchive(aname)
								if complain(name, err) {
									continue
								}

								if a == nil {
									a, err = db.InsertArchive(aname)
									if complain(name, err) {
										continue
									}
								}
							}

							f, err := db.QueryFile(a, file.Name)
							if complain(file.Name, err) {
								continue
							}

							if f == nil {
								f, err = db.InsertFile(a, file.Name)
								if complain(file.Name, err) {
									continue
								}
							}

							collisions := make(map[string]int)

							db.Begin()
							for _, p := range t.Pairs {
								collision := collisions[p.Identifier]
								collisions[p.Identifier] = collision + 1

								s, err := db.QueryString(f, collision, p.Identifier)
								if complain(f.Name + ": " + p.Identifier, err) {
									break
								}

								if s != nil {
									if s.Value != p.String {
										if flagTrans != "" {
											if translation == nil {
												translation, err = db.QueryTranslation(flagTrans)
												if translation == nil {
													translation, err = db.InsertTranslation(flagTrans)
													if complain(flagTrans, err) {
														break
													}
												}
											}

											ts, _ := db.QueryTranslationString(translation, s)
											if ts != nil {
												_, err = db.UpdateTranslationString(ts, p.String)
											} else {
												_, err = db.InsertTranslationString(translation, s, p.String)
											}
										} else {
											_, err = db.UpdateString(s, flagImport, p.String)
										}

										if complain(f.Name + ": " + p.Identifier, err) {
											break
										}
									}
								} else {
									if flagTrans != "" {
										complain(f.Name + ": " + p.Identifier + ": " + p.String, errors.New("translated identifier does not exist"))
									} else {
										_, err := db.InsertString(f, flagImport, collision, p.Identifier, p.String)
										if complain(f.Name + ": " + p.Identifier, err) {
											break
										}
									}
								}
							}
							db.End()
						}
					}
				}

				af.Close()
			}
		}
	} else if flagTrans != "" {
		if flag.NArg() < 2 {
			fmt.Fprintln(os.Stderr, "no pso2 dir provided")
			return
		}

		if flagBackup != "" {
			err := os.MkdirAll(flagBackup, 0777)
			ragequit(flagBackup, err)
		}

		pso2dir := flag.Arg(1)
		if flagOutput == "" {
			flagOutput = pso2dir
		} else {
			err := os.MkdirAll(flagOutput, 0777)
			ragequit(flagOutput, err)
		}

		errs := cmd.PatchFiles(db, pso2dir, flagTrans, flagBackup, flagOutput, flagParallel)

		for _, err := range errs {
			complain("", err)
		}
	}

	db.Close()

	if flagStrip != "" {
		err := cmd.StripDatabase(dbpath, flagStrip)
		complain(flagStrip, err)
	}
}
