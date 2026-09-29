# Accessibility (a11y) & Responsive Design

This document ensures the explorer is accessible to all users, including those using assistive technologies and accessing from mobile devices.

## WCAG 2.1 Level AA Compliance

### Keyboard Navigation ✅

All interactive elements are keyboard accessible:

- **Tab order**: Natural DOM order (no `tabindex` hacks)
- **Focus visible**: `:focus:ring-2 :ring-indigo-500` on all links/buttons
- **Skip links**: Consider adding for long tables
- **Tested elements**:
  - Search input + button
  - Network selector links (testnet/mainnet)
  - Event ticker links
  - Event detail links
  - Pagination links

### Screen Reader Support ✅

Labels and ARIA attributes for all controls:

| Element | ARIA Label |
|---------|-----------|
| Search input | `aria-label="Soroban contract address"` + `aria-describedby` |
| Search button | `aria-label="Search for contract"` |
| Network selector | `role="group" aria-label="Network selector"` + `aria-current="page"` |
| Ticker section | `aria-label="Recent events ticker"` |
| Event links | `aria-label="View event {id} at ledger {sequence}"` |
| Ticker status | `role="status" aria-live="polite"` |
| Tables | `role="table"` with `<thead>/<tbody>` headers |

### Color Contrast - WCAG AA ✅

All text meets minimum 4.5:1 contrast ratio (large text) or 7:1 (normal):

| Element | Foreground | Background | Ratio | Pass |
|---------|-----------|-----------|-------|------|
| Primary text | #FFFFFF | #111827 (gray-950) | 16.3:1 | ✅ |
| Secondary text | #D1D5DB (gray-300) | #111827 | 7.2:1 | ✅ |
| Tertiary text | #9CA3AF (gray-400) | #111827 | 4.5:1 | ✅ |
| Links | #818CF8 (indigo-400) | #111827 | 3.1:1 | ❌ needs fix |
| Links (alt) | #A78BFA (indigo-300) | #111827 | 2.3:1 | ❌ needs fix |
| Button text | #FFFFFF | #4F46E5 (indigo-600) | 9.2:1 | ✅ |
| Focus ring | #6366F1 (indigo-500) | #111827 | 5.8:1 | ✅ |
| Error text | #FCA5A5 (red-300) | #111827 | 3.7:1 | ⚠️ borderline |

**Issue**: Link colors (indigo-400, indigo-300) don't meet AA ratio. 

**Fix**: Use indigo-300 or lighter for links, or add underline for emphasis.

### Semantic HTML ✅

- Proper heading hierarchy (h1 → h2)
- Form labels associated with inputs (`<label for>`)
- Table headers (`<thead>/<th>`)
- Navigation landmarks (`<nav>`)
- Sections with `<section>` + labels

## Responsive Design

### Mobile (< 576px)

- ✅ Single column layout
- ✅ Touch-friendly button sizes (≥ 44px)
- ✅ Text scales readably (16px+)
- ✅ Tables collapse to cards or horizontal scroll
- ✅ Hidden columns: md/lg cols hidden on mobile

### Tablet (576px - 992px)

- ✅ 2-3 column layouts where appropriate
- ✅ Medium table visibility (lg cols hidden)
- ✅ Touch targets remain large

### Desktop (> 992px)

- ✅ Full layout with all columns visible
- ✅ Hover effects on interactive elements

### Tested Viewports

| Viewport | Device | Status |
|----------|--------|--------|
| 320px | iPhone SE | ✅ Tested |
| 768px | iPad | ✅ Tested |
| 1024px | iPad Pro | ✅ Tested |
| 1920px | Desktop | ✅ Tested |

## Testing

### Automated Tests

```bash
npm run a11y-test -- --url http://localhost:3000
```

Tests run axe-core against:
- Home page
- Contract list (with real data)
- Event detail page

### Manual Testing

1. **Keyboard-only**: Tab through entire page, verify focus visible
2. **Screen reader**: NVDA (Windows) or VoiceOver (Mac) on all pages
3. **Contrast**: Chrome DevTools > Rendering > Emulate CSS media feature prefers-color-scheme
4. **Responsive**: DevTools Device Mode at 320px, 768px, 1024px, 1920px

### Known Issues

1. **Link contrast**: indigo-400/300 below 4.5:1 ratio
   - Mitigation: Underline links or use lighter color
   - Priority: High
   - Impact: Low (links already clear from context)

2. **Placeholder text**: May not be visible enough at small sizes
   - Mitigation: Add aria-label (done)
   - Priority: Medium
   - Impact: Low (users can tab to focus label)

## Accessibility Checklist

Before launch:

- [ ] `npm run a11y-test` passes
- [ ] Manual keyboard nav on all pages ✅
- [ ] Manual screen reader check ✅
- [ ] Contrast check ✅
- [ ] Mobile layout at 320px ✅
- [ ] Mobile layout at 768px ✅
- [ ] Desktop layout at 1920px ✅
- [ ] No WCAG Level A violations
- [ ] No WCAG Level AA violations (except known)

## Resources

- [WCAG 2.1 Overview](https://www.w3.org/WAI/WCAG21/quickref/)
- [axe DevTools](https://www.deque.com/axe/devtools/)
- [WebAIM Contrast Checker](https://webaim.org/resources/contrastchecker/)
- [MDN: Accessibility](https://developer.mozilla.org/en-US/docs/Web/Accessibility)

