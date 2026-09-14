// selection.ts — ONE way to say "this is the one you picked".
//
// There used to be seven idioms across the app: a tinted fill, a tinted fill
// plus a border, a ring, a background swap with a shadow, an opacity bar, a
// text-size change, and a pill. Nothing transferred between screens, so there
// was no signal to learn, and in the light theme every one of them landed
// between 1.08 and 1.62 against its unselected neighbour.
//
// The pattern that works is the play card's: a FULL-OPACITY ring in --primary
// drawn OUTSIDE the box, a filled pill, and a change of content. Only one of
// those is colour, which is why it survives both themes, a phone in sunlight,
// and anyone who does not separate hues. A ring at full --primary reads at any
// lightness; a 10% fill never will.

/** selectionClass is the whole idiom. Pass it the state and nothing else. */
export function selectionClass(selected: boolean): string {
  return selected ? "af-selected" : "af-unselected";
}

/** SELECTED_RING is the ring on its own, for a control that already owns its
 *  fill (a tab, a nav item) and only needs the outline. */
export const SELECTED_RING = "af-selected";
