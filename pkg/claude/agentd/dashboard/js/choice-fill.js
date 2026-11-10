// choice-fill.js — how a type-to-filter choice (choice-combobox.js) and an
// agent's form fill (form-fill.js) talk, kept apart from both so that
// form-fill stays free of Preact (tcl-alhauf).
//
//   CHOICE_FIELD       what marks a choice that is a field: its wrapper, which
//                      holds the value in data-value and its rows in choiceItems
//   CHOICE_FILL_EVENT  sent to the wrapper (detail: a key, or '' for none): a
//                      fill choosing, as a pick would
//   CHOICE_PICK_EVENT  sent from the wrapper (bubbling): the operator choosing

export const CHOICE_FIELD = '[data-fill-choice]';
export const CHOICE_FILL_EVENT = 'choice-fill';
export const CHOICE_PICK_EVENT = 'choice-pick';
