package datagen

import "errors"

// WriteEvalDatasets writes the labelled eval datasets (implemented in T-33).
func WriteEvalDatasets(_ string, _ uint64) (int, error) {
	return 0, errors.New("eval datasets are not implemented yet")
}
