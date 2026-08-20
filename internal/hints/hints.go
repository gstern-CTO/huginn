package hints

import "fmt"

// Hints are generated server-side from the shape of the result, so the agent is
// told what to investigate next without having to reason it out. They are the
// difference between a research tool and a raw API wrapper.

func CodeSearch(matchCount int, topFile string, concise bool) []string {
	if matchCount == 0 {
		return []string{
			"Try broader keywords, or remove the extension filter.",
			"Try match=path to search filenames instead of file content.",
			"Use repository search first to confirm the repository contains the subject at all.",
		}
	}
	hints := []string{}
	if topFile != "" {
		hints = append(hints, fmt.Sprintf("Use github_file_content to read %s.", topFile))
	}
	if concise {
		hints = append(hints, "This was a concise (paths-only) search; re-run with concise=false on the narrowed query to see matched content.")
	} else {
		hints = append(hints, "Use lsp_navigate with operation=references to trace where these symbols are called from.")
	}
	if matchCount >= 20 {
		hints = append(hints, "Many matches: add an owner, repo, or path filter to narrow the landscape.")
	}
	return hints
}

func FileContent(path string, partial bool) []string {
	hints := []string{
		"Use lsp_navigate with operation=definition to trace any symbol in this file.",
		fmt.Sprintf("Use github_search_pull_requests with deepRead=true to find when %s last changed and why.", path),
	}
	if partial {
		hints = append([]string{"This is a partial read; request an adjacent line range to see more."}, hints...)
	}
	return hints
}

func RepoStructure(hasMore bool, depth int) []string {
	hints := []string{}
	if hasMore {
		hints = append(hints, "The tree was truncated: request the next page, or narrow with a subdirectory path.")
	}
	if depth <= 1 {
		hints = append(hints, "Increase depth to see nested directories, or pass a path to descend into one.")
	}
	hints = append(hints, "Use github_search_code scoped to this repository to find where a concept is implemented.")
	return hints
}

func RepoSearch(count int) []string {
	if count == 0 {
		return []string{
			"No repositories matched. Drop the language or star filters and retry with keywords alone.",
			"Topics and keywords are searched separately; try one topic on its own.",
		}
	}
	return []string{
		"Use github_repo_structure on the most promising repository to see how it is laid out.",
		"Use github_search_code with an owner/repo filter to search inside a specific result.",
	}
}

func PRSearch(count int, deepRead bool) []string {
	if count == 0 {
		return []string{
			"No pull requests matched. Widen the state filter to 'all', or drop the author filter.",
			"Try keywords from the commit message rather than the code identifier.",
		}
	}
	hints := []string{}
	if !deepRead {
		hints = append(hints, "Re-run with deepRead=true on a specific PR number to see the changed files and patches.")
	} else {
		hints = append(hints, "Use github_file_content on a changed file to read its current state and compare against the patch.")
	}
	hints = append(hints, "Use github_search_code to find whether the pattern introduced here appears elsewhere.")
	return hints
}

func LocalSearch(matchCount int, topFile string, discovery bool) []string {
	if matchCount == 0 {
		return []string{
			"No matches. Try a broader pattern, or set regex=false to search literally.",
			"Use local_find_files to confirm the files you expect are actually in the workspace.",
		}
	}
	hints := []string{}
	if discovery {
		hints = append(hints, "This was a discovery pass (counts only). Re-run without discovery on the densest file to see the matching lines.")
	}
	if topFile != "" {
		hints = append(hints, fmt.Sprintf("Use local_file_content to read %s.", topFile))
	}
	hints = append(hints, "Use lsp_navigate with operation=references for call-site accuracy that regex cannot give.")
	return hints
}

func LocalFile(path string, partial bool) []string {
	hints := []string{
		"Use lsp_navigate with operation=documentSymbol to list every symbol declared in this file.",
	}
	if partial {
		hints = append([]string{"This is a partial read; request an adjacent line range to see more."}, hints...)
	}
	hints = append(hints, fmt.Sprintf("Use local_search_code scoped to the directory containing %s to find related code.", path))
	return hints
}

func FindFiles(count int, hasMore bool) []string {
	if count == 0 {
		return []string{
			"Nothing matched. Loosen the name pattern, or drop the extension filter.",
			"Use local_directory_structure to see what the workspace actually contains.",
		}
	}
	hints := []string{}
	if hasMore {
		hints = append(hints, "Results were capped: narrow the pattern or raise the limit to see the rest.")
	}
	hints = append(hints, "Use local_search_code across these files to find the relevant one by content.")
	return hints
}

func DirectoryStructure(entryCount int, hasMore bool) []string {
	hints := []string{}
	if hasMore {
		hints = append(hints, "The listing was truncated: request the next page, or descend into a subdirectory.")
	}
	if entryCount == 0 {
		return []string{"This directory is empty. Check the parent directory instead."}
	}
	hints = append(hints,
		"Use local_search_code scoped to this directory to find where a concept is implemented.",
		"Use local_find_files with an extension filter to isolate one language.",
	)
	return hints
}

func LSP(operation string, resultCount int, usedFallback bool) []string {
	hints := []string{}
	if usedFallback {
		hints = append(hints, "These results came from a ripgrep symbol fallback, not a language server: they are textual matches and may include false positives.")
	}
	switch {
	case resultCount == 0:
		hints = append(hints,
			"No results. Confirm the symbol name and the 1-based line/character position point at the identifier itself.",
			"Use local_search_code to locate the exact declaration position first, then retry.",
		)
	case operation == "references" && resultCount > 20:
		hints = append(hints, "Too many references — narrow with a more specific symbol, or filter by directory.")
	case operation == "definition":
		hints = append(hints, "Use local_file_content on the definition site with a line range to read the implementation.")
	case operation == "documentSymbol":
		hints = append(hints, "Use lsp_navigate with operation=references on any symbol above to see who calls it.")
	default:
		hints = append(hints, "Use local_file_content with a line range to read each result in context.")
	}
	return hints
}

func Databricks(rowCount int, env string, truncated bool) []string {
	if rowCount == 0 {
		return []string{
			"No rows. Try a longer time range, or check the table name with SHOW TABLES.",
			fmt.Sprintf("This query ran against the %s environment; the data you expect may live elsewhere.", env),
		}
	}
	hints := []string{}
	if truncated {
		hints = append(hints, "The row cap was reached: add a LIMIT with an offset, or aggregate in SQL rather than returning raw rows.")
	}
	hints = append(hints,
		"Aggregate in SQL rather than post-processing rows: it is faster and returns far fewer tokens.",
	)
	if env == "dev" {
		hints = append(hints, "This is dev data. Pass env=prod explicitly if production telemetry is what you need.")
	}
	return hints
}

// BudgetExhausted is appended whenever the token budget stopped a response
// short, so the agent knows the result set is incomplete by policy rather than
// because the data ran out.
func BudgetExhausted() string {
	return "The response hit its token budget and was truncated. Paginate, narrow the query, or raise MAX_RESPONSE_TOKENS."
}
