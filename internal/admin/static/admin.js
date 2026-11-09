// The admin page's one script, and all it does: dates and times in the owner's
// own time zone, Copy with its fallback, and the relay's range to forward kept up
// with the games at once as they are typed. Everything else on the page is plain
// HTML that works without it.
//
// A container's clock is usually UTC, and the person reading the message may be
// somewhere else, so a time names its zone. Plain HTTP, which is how a home
// network reaches this page, has no clipboard API, so Copy falls back to the old
// way, then to selecting the text for the owner to copy by hand.
(function () {
  var date = new Intl.DateTimeFormat('en-US', { month: 'long', day: 'numeric' });
  var short = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric', hour: 'numeric', minute: '2-digit', timeZoneName: 'short' });

  var day = new Intl.DateTimeFormat('en-US', { month: 'short', day: 'numeric' });

  function words(format, date, join) {
    var p = {};
    format.formatToParts(date).forEach(function (part) { p[part.type] = part.value; });
    return p.month + ' ' + p.day + join + p.hour + ':' + p.minute + ' ' + p.dayPeriod + ' ' + p.timeZoneName;
  }

  document.querySelectorAll('time[datetime]').forEach(function (t) {
    var d = new Date(t.getAttribute('datetime'));
    if (t.hasAttribute('data-day')) t.textContent = day.format(d);
    else if (t.hasAttribute('data-date')) t.textContent = date.format(d);
    else t.textContent = words(short, d, ', ');
  });

  // Each relayed game takes two ports, right after the relay's own.
  document.querySelectorAll('[data-games]').forEach(function (input) {
    var first = Number(input.getAttribute('data-games'));
    var last = input.form.querySelector('[data-last]');
    input.addEventListener('input', function () {
      var games = Math.min(Math.max(Math.floor(Number(input.value)) || 1, 1), Number(input.max));
      last.textContent = first + 2 * games;
    });
  });

  document.querySelectorAll('[data-message]').forEach(function (box) {
    var pre = box.querySelector('pre');
    var text = pre.textContent;
    var status = box.querySelector('[data-copied]');

    function selectIt() {
      var range = document.createRange();
      range.selectNodeContents(pre);
      var selection = window.getSelection();
      selection.removeAllRanges();
      selection.addRange(range);
      var copied = false;
      try { copied = document.execCommand('copy'); } catch (e) { copied = false; }
      status.textContent = copied ? 'Copied' : 'Selected. Copy it by hand';
    }

    box.querySelector('[data-copy]').addEventListener('click', function () {
      if (navigator.clipboard && window.isSecureContext) {
        navigator.clipboard.writeText(text).then(function () { status.textContent = 'Copied'; }, selectIt);
      } else {
        selectIt();
      }
    });
  });
})();
